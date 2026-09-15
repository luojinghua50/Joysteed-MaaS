package rediskv_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rediskv"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/kvstore"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// This file tests the property that motivates the whole deliverable, and that
// the conformance suite deliberately cannot: behaviour across SEPARATE store
// instances, i.e. separate gateway processes.
//
// The conformance suite hands every assertion one store instance, so the
// in-memory store passes all of it. That is correct — per process, it behaves
// identically. It just does not coordinate anything between processes, which is
// exactly what session stickiness, the routing warm claim and the job sweeper's
// poll lease need it to do.

// sharedRedisNodes returns n independent Stores over separate clients on ONE
// key prefix — two processes pointed at one Redis.
func sharedRedisNodes(t *testing.T, n int) []schemas.KVStore {
	t.Helper()
	prefix := fmt.Sprintf("xnode:%s:%d:", t.Name(), time.Now().UnixNano())

	nodes := make([]schemas.KVStore, 0, n)
	for range n {
		// A separate client per node: separate connection pool, as two processes
		// would have. Sharing one client would test goroutines, not nodes.
		client := redis.NewClient(&redis.Options{Addr: redisAddr(), Protocol: 3})
		t.Cleanup(func() { _ = client.Close() })
		store := rediskv.NewWithClient(client, 2*time.Second, prefix)
		require.NoError(t, store.Ping())
		nodes = append(nodes, store)
	}
	return nodes
}

// separateMemNodes returns n independent in-memory stores — the status quo,
// where each process has its own map.
func separateMemNodes(t *testing.T, n int) []schemas.KVStore {
	t.Helper()
	nodes := make([]schemas.KVStore, 0, n)
	for range n {
		s, err := kvstore.New(kvstore.Config{})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		nodes = append(nodes, s)
	}
	return nodes
}

// TestCrossNode_RedisGivesExactlyOneWinner is the deliverable's core claim: two
// processes racing for the same claim key, exactly one wins.
func TestCrossNode_RedisGivesExactlyOneWinner(t *testing.T) {
	nodes := sharedRedisNodes(t, 2)

	setA, err := nodes[0].SetNXWithTTL("warm-claim", "node-a", time.Minute)
	require.NoError(t, err)
	setB, err := nodes[1].SetNXWithTTL("warm-claim", "node-b", time.Minute)
	require.NoError(t, err)

	require.True(t, setA, "first node must win")
	require.False(t, setB, "second node must lose — this is what makes the claim a claim")

	// The loser must also be able to SEE the winner's value, which is what
	// ClaimHeld (complexitykvwarmcoordinator.go:82) relies on.
	raw, err := nodes[1].Get("warm-claim")
	require.NoError(t, err, "loser must observe the winner's claim")
	got, ok := decodeString(raw)
	require.True(t, ok)
	require.Equal(t, "node-a", got)
}

// TestCrossNode_InMemoryGivesTwoWinners documents the defect being fixed. It
// asserts the BROKEN behaviour on purpose: both nodes believe they hold an
// exclusive claim, and neither can see the other.
//
// Without this test the Redis test above proves only "Redis works", not
// "Redis fixes something". Pinning the old behaviour also means that if
// upstream ever makes the in-memory store cluster-aware, this test fails and
// tells us the workaround may no longer be needed.
func TestCrossNode_InMemoryGivesTwoWinners(t *testing.T) {
	nodes := separateMemNodes(t, 2)

	setA, err := nodes[0].SetNXWithTTL("warm-claim", "node-a", time.Minute)
	require.NoError(t, err)
	setB, err := nodes[1].SetNXWithTTL("warm-claim", "node-b", time.Minute)
	require.NoError(t, err)

	require.True(t, setA)
	require.True(t, setB,
		"in-memory: both nodes win the same claim — the defect Phase 1 item 4 exists to fix")

	_, err = nodes[0].Get("warm-claim")
	require.NoError(t, err)
	rawB, err := nodes[1].Get("warm-claim")
	require.NoError(t, err)
	gotB, ok := decodeString(rawB)
	require.True(t, ok)
	require.Equal(t, "node-b", gotB, "each node sees only its own write")
}

// TestCrossNode_RedisSessionStickinessIsShared covers the D8 case: a request
// landing on node B must resolve to the key node A already pinned, otherwise
// stickiness silently degrades to per-node affinity under a round-robin load
// balancer.
func TestCrossNode_RedisSessionStickinessIsShared(t *testing.T) {
	nodes := sharedRedisNodes(t, 3)

	const sessionKey = "session-sticky:abc123"
	require.NoError(t, nodes[0].SetWithTTL(sessionKey, "key-id-7", schemas.DefaultSessionStickyTTL))

	for i, node := range nodes {
		raw, err := node.Get(sessionKey)
		require.NoErrorf(t, err, "node %d must see the pinned key", i)
		got, ok := decodeString(raw)
		require.True(t, ok)
		require.Equalf(t, "key-id-7", got, "node %d resolved a different key", i)
	}
}

// TestCrossNode_ConcurrentClaimHasExactlyOneWinner is the contended version:
// 32 goroutines across 4 nodes racing one key. Serial calls could pass by
// accident of ordering; this cannot.
func TestCrossNode_ConcurrentClaimHasExactlyOneWinner(t *testing.T) {
	nodes := sharedRedisNodes(t, 4)

	const goroutinesPerNode = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)

	start := make(chan struct{})
	for nodeIdx, node := range nodes {
		for g := range goroutinesPerNode {
			wg.Add(1)
			go func(node schemas.KVStore, id string) {
				defer wg.Done()
				<-start // release together, to maximise contention
				set, err := node.SetNXWithTTL("contended", id, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if set {
					mu.Lock()
					winners = append(winners, id)
					mu.Unlock()
				}
			}(node, fmt.Sprintf("node-%d-g%d", nodeIdx, g))
		}
	}
	close(start)
	wg.Wait()

	require.Len(t, winners, 1,
		"exactly one winner expected across %d nodes × %d goroutines, got %v",
		len(nodes), goroutinesPerNode, winners)

	raw, err := nodes[0].Get("contended")
	require.NoError(t, err)
	got, ok := decodeString(raw)
	require.True(t, ok)
	require.Equal(t, winners[0], got, "stored value must be the winner's")
}

// TestCrossNode_ReleaseAllowsReclaim covers the claim lifecycle the routing
// coordinator uses: Claim → Release → another node can Claim.
func TestCrossNode_ReleaseAllowsReclaim(t *testing.T) {
	nodes := sharedRedisNodes(t, 2)

	set, err := nodes[0].SetNXWithTTL("lease", "node-a", time.Minute)
	require.NoError(t, err)
	require.True(t, set)

	removed, err := nodes[0].Delete("lease")
	require.NoError(t, err)
	require.True(t, removed)

	set, err = nodes[1].SetNXWithTTL("lease", "node-b", time.Minute)
	require.NoError(t, err)
	require.True(t, set, "release by one node must free the claim for another")
}

// TestCrossNode_ExpiredClaimIsReclaimable covers lease expiry, which is how the
// job sweeper recovers from a node dying mid-poll without releasing.
func TestCrossNode_ExpiredClaimIsReclaimable(t *testing.T) {
	nodes := sharedRedisNodes(t, 2)

	set, err := nodes[0].SetNXWithTTL("short-lease", "node-a", 150*time.Millisecond)
	require.NoError(t, err)
	require.True(t, set)

	set, err = nodes[1].SetNXWithTTL("short-lease", "node-b", time.Minute)
	require.NoError(t, err)
	require.False(t, set, "must not be claimable while the lease is live")

	require.Eventually(t, func() bool {
		set, err := nodes[1].SetNXWithTTL("short-lease", "node-b", time.Minute)
		return err == nil && set
	}, 3*time.Second, 50*time.Millisecond,
		"an expired lease must become claimable by another node")
}
