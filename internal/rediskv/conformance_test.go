package rediskv_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rediskv"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/kvstore"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// The suite below runs the SAME assertions against the in-memory store and the
// Redis store, because the whole deliverable rests on one claim: a Redis store
// can be dropped into schemas.BifrostConfig.KVStore without any in-tree
// consumer noticing. A test that only exercised the Redis store would verify
// that it works, not that it is interchangeable.
//
// One thing deliberately NOT asserted: that Get returns the same Go type from
// both. It cannot. The in-memory store hands back the value it was given; Redis
// can only hand back bytes. Asserting type equality would be asserting a
// property no cross-process store can have. So every assertion here goes
// through the decode logic real consumers use — that is the actual contract.
//
// Requires Redis:
//
//	docker run -d --name maas-kv-redis -p 56379:6379 redis:7-alpine
//
// Teardown: docker rm -f maas-kv-redis
// Override with MAAS_KV_REDIS_ADDR.

func redisAddr() string {
	if v := os.Getenv("MAAS_KV_REDIS_ADDR"); v != "" {
		return v
	}
	return "localhost:56379"
}

// newRedisStore returns a Store on a unique key prefix so parallel tests and
// repeat runs cannot observe each other's keys.
func newRedisStore(t *testing.T) *rediskv.Store {
	t.Helper()
	addr := redisAddr()
	client := redis.NewClient(&redis.Options{Addr: addr, Protocol: 3})
	t.Cleanup(func() { _ = client.Close() })

	store := rediskv.NewWithClient(client, 2*time.Second,
		fmt.Sprintf("test:%s:%d:", t.Name(), time.Now().UnixNano()))
	require.NoError(t, store.Ping(),
		"start Redis first: docker run -d --name maas-kv-redis -p 56379:6379 redis:7-alpine")
	return store
}

func newMemStore(t *testing.T) schemas.KVStore {
	t.Helper()
	s, err := kvstore.New(kvstore.Config{CleanupInterval: 50 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// implementations is the table every conformance test runs over.
func implementations(t *testing.T) map[string]schemas.KVStore {
	t.Helper()
	return map[string]schemas.KVStore{
		"memory": newMemStore(t),
		"redis":  newRedisStore(t),
	}
}

// decodeString mirrors, verbatim in structure, what all three in-tree consumers
// do with a Get result: accept a string, or accept bytes and unmarshal them
// falling back to the raw string. Copied rather than imported because the real
// ones are unexported (core/bifrost.go:9336,
// plugins/routing/complexitysession.go:131,
// complexitykvwarmcoordinator.go:125). If this decodes correctly for both
// stores, those consumers work against both stores.
func decodeString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case []byte:
		var s string
		if err := json.Unmarshal(typed, &s); err != nil {
			return string(typed), true
		}
		return s, true
	default:
		return "", false
	}
}

func TestConformance_RoundTripString(t *testing.T) {
	for name, store := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, store.SetWithTTL("k1", "complex", time.Minute))

			raw, err := store.Get("k1")
			require.NoError(t, err)

			got, ok := decodeString(raw)
			require.True(t, ok, "consumer decode rejected type %T", raw)
			require.Equal(t, "complex", got)
		})
	}
}

// TestConformance_MissReturnsFrameworkSentinel is the single most
// load-bearing assertion in this file. complexitysession.go:52 does
// errors.Is(err, kvstore.ErrNotFound) to tell "new session" from "store broke".
// A store returning its own miss sentinel would turn every cache miss into a
// failed request — and would still pass a test that only checked err != nil.
func TestConformance_MissReturnsFrameworkSentinel(t *testing.T) {
	for name, store := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			_, err := store.Get("absent")
			require.ErrorIs(t, err, kvstore.ErrNotFound)
		})
	}
}

func TestConformance_SetNXSemantics(t *testing.T) {
	for name, store := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			set, err := store.SetNXWithTTL("claim", "node-a", time.Minute)
			require.NoError(t, err)
			require.True(t, set, "first claim must win")

			set, err = store.SetNXWithTTL("claim", "node-b", time.Minute)
			require.NoError(t, err)
			require.False(t, set, "second claim must lose")

			raw, err := store.Get("claim")
			require.NoError(t, err)
			got, ok := decodeString(raw)
			require.True(t, ok)
			require.Equal(t, "node-a", got, "loser must not overwrite")
		})
	}
}

func TestConformance_Delete(t *testing.T) {
	for name, store := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, store.SetWithTTL("gone", "v", time.Minute))

			removed, err := store.Delete("gone")
			require.NoError(t, err)
			require.True(t, removed)

			removed, err = store.Delete("gone")
			require.NoError(t, err)
			require.False(t, removed, "second delete reports absent, not error")

			_, err = store.Get("gone")
			require.ErrorIs(t, err, kvstore.ErrNotFound)
		})
	}
}

func TestConformance_TTLExpiry(t *testing.T) {
	for name, store := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, store.SetWithTTL("brief", "v", 150*time.Millisecond))

			_, err := store.Get("brief")
			require.NoError(t, err, "must be present before expiry")

			require.Eventually(t, func() bool {
				_, err := store.Get("brief")
				return err != nil
			}, 3*time.Second, 50*time.Millisecond, "must expire")

			_, err = store.Get("brief")
			require.ErrorIs(t, err, kvstore.ErrNotFound,
				"expired key must read as a miss, not a different error")
		})
	}
}

func TestConformance_InputValidation(t *testing.T) {
	for name, store := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			_, err := store.Get("")
			require.ErrorIs(t, err, kvstore.ErrEmptyKey)

			require.ErrorIs(t, store.SetWithTTL("", "v", time.Minute), kvstore.ErrEmptyKey)
			require.ErrorIs(t, store.SetWithTTL("k", "v", -time.Second), kvstore.ErrInvalidTTL)

			_, err = store.SetNXWithTTL("", "v", time.Minute)
			require.ErrorIs(t, err, kvstore.ErrEmptyKey)
			_, err = store.SetNXWithTTL("k", "v", -time.Second)
			require.ErrorIs(t, err, kvstore.ErrInvalidTTL)

			_, err = store.Delete("")
			require.ErrorIs(t, err, kvstore.ErrEmptyKey)
		})
	}
}

// TestConformance_ZeroTTLMeansNoExpiration pins the framework's documented
// ttl=0 semantics ("ttl=0 means no expiration"), which is the opposite of what
// a naive Redis mapping would do if it treated 0 as "expire immediately".
func TestConformance_ZeroTTLMeansNoExpiration(t *testing.T) {
	for name, store := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, store.SetWithTTL("forever", "v", 0))
			time.Sleep(200 * time.Millisecond)
			_, err := store.Get("forever")
			require.NoError(t, err, "ttl=0 must not expire")
		})
	}
}
