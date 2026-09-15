package rediskv_test

import (
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/framework/kvstore"
	"github.com/stretchr/testify/require"
)

// The conformance suite decodes through decodeString, which MIRRORS consumer
// logic rather than being it. That leaves one gap: if the mirror drifts from the
// real thing, those tests stay green while production breaks.
//
// This file closes the gap without needing to construct a whole plugin, by
// checking the wire format against upstream's OWN code path. framework/kvstore
// marshals with sonic.Marshal before handing bytes to SyncDelegate.OnSet
// (kvstore.go:135), and reconstructs them on the receiving side via SetRemote
// (kvstore.go:196). So if the bytes this store puts in Redis are accepted by
// SetRemote and decode to the same value, then this store's wire format IS
// upstream's gossip wire format — which is precisely the format every consumer's
// []byte branch was written to parse.

// TestWireFormat_MatchesUpstreamGossipEncoding asserts byte-level identity with
// what upstream would have gossiped for the same value.
func TestWireFormat_MatchesUpstreamGossipEncoding(t *testing.T) {
	store := newRedisStore(t)

	cases := []struct {
		name  string
		value any
	}{
		{"string", "key-id-7"},
		{"tier", "complex"},
		{"warm record", "384|ns-abc"},
		{"int", 42},
		{"bool", true},
		{"struct", struct {
			Dimension int    `json:"dimension"`
			Namespace string `json:"namespace"`
		}{384, "ns-abc"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, store.SetWithTTL("wire", tc.value, time.Minute))

			raw, err := store.Get("wire")
			require.NoError(t, err)
			got, ok := raw.([]byte)
			require.True(t, ok, "Get must return []byte, got %T", raw)

			// What upstream's SyncDelegate.OnSet would have carried.
			want, err := sonic.Marshal(tc.value)
			require.NoError(t, err)
			require.Equal(t, string(want), string(got),
				"wire format diverges from upstream's gossip encoding")
		})
	}
}

// TestWireFormat_UpstreamSetRemoteAcceptsOurBytes feeds this store's bytes into
// the in-memory store's gossip-receive path. It passing means an in-memory node
// can consume what a Redis-backed node wrote, using upstream's real decoder
// rather than a copy of it.
func TestWireFormat_UpstreamSetRemoteAcceptsOurBytes(t *testing.T) {
	store := newRedisStore(t)

	mem, err := kvstore.New(kvstore.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mem.Close() })

	require.NoError(t, store.SetWithTTL("shared", "key-id-7", time.Minute))
	raw, err := store.Get("shared")
	require.NoError(t, err)
	payload, ok := raw.([]byte)
	require.True(t, ok)

	now := time.Now().UnixNano()
	require.NoError(t, mem.SetRemote("shared", payload, now, now+int64(time.Minute)),
		"upstream's gossip-receive path must accept our bytes")

	// Read back through the in-memory store and decode as consumers do.
	memRaw, err := mem.Get("shared")
	require.NoError(t, err)
	got, ok := decodeString(memRaw)
	require.True(t, ok, "consumer decode rejected type %T", memRaw)
	require.Equal(t, "key-id-7", got,
		"value must survive the Redis-write → gossip-receive → consumer-decode path")
}

// TestWireFormat_RegisteredDecoderReconstructsOurBytes exercises the
// RegisterDecoder seam (kvstore.go:80) against this store's output, confirming
// the typed-reconstruction path upstream built for gossip also works on bytes
// this store produced.
func TestWireFormat_RegisteredDecoderReconstructsOurBytes(t *testing.T) {
	store := newRedisStore(t)

	type warmRecord struct {
		Dimension int    `json:"dimension"`
		Namespace string `json:"namespace"`
	}

	mem, err := kvstore.New(kvstore.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mem.Close() })

	mem.RegisterDecoder("warm:", func(data []byte) (any, error) {
		var rec warmRecord
		if err := sonic.Unmarshal(data, &rec); err != nil {
			return nil, err
		}
		return rec, nil
	})

	want := warmRecord{Dimension: 384, Namespace: "ns-abc"}
	require.NoError(t, store.SetWithTTL("warm:gen1", want, time.Minute))

	raw, err := store.Get("warm:gen1")
	require.NoError(t, err)
	payload, ok := raw.([]byte)
	require.True(t, ok)

	now := time.Now().UnixNano()
	require.NoError(t, mem.SetRemote("warm:gen1", payload, now, 0))

	got, err := mem.Get("warm:gen1")
	require.NoError(t, err)
	require.Equal(t, want, got,
		"a registered decoder must reconstruct the concrete type from our bytes")
}
