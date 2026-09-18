package rediskv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/kvstore"
	"github.com/redis/go-redis/v9"
)

// Store is a Redis-backed schemas.KVStore.
//
// Compile-time proof it satisfies the interface the core injects.
var _ schemas.KVStore = (*Store)(nil)

type Store struct {
	client    redis.UniversalClient
	opTimeout time.Duration
	keyPrefix string

	decoderMu sync.RWMutex
	decoders  map[string]kvstore.TypeDecoder
}

// New connects and verifies reachability before returning, matching the
// eager-Ping convention of the framework's own store constructors.
func New(c *Config) (*Store, error) {
	if err := Validate(c); err != nil {
		return nil, err
	}
	client, err := newClient(c)
	if err != nil {
		return nil, err
	}

	opTimeout := time.Duration(c.OpTimeout)
	if opTimeout <= 0 {
		opTimeout = defaultOpTimeout
	}
	keyPrefix := c.KeyPrefix
	if keyPrefix == "" {
		keyPrefix = DefaultKeyPrefix
	}

	s := &Store{client: client, opTimeout: opTimeout, keyPrefix: keyPrefix, decoders: make(map[string]kvstore.TypeDecoder)}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("rediskv: failed to connect to redis: %w", err)
	}
	return s, nil
}

// NewWithClient builds a Store over an existing client. Used by tests and by
// callers that already own a Redis client they want to share.
func NewWithClient(client redis.UniversalClient, opTimeout time.Duration, keyPrefix string) *Store {
	if opTimeout <= 0 {
		opTimeout = defaultOpTimeout
	}
	if keyPrefix == "" {
		keyPrefix = DefaultKeyPrefix
	}
	return &Store{client: client, opTimeout: opTimeout, keyPrefix: keyPrefix, decoders: make(map[string]kvstore.TypeDecoder)}
}

func (s *Store) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.opTimeout)
}

func (s *Store) k(key string) string { return s.keyPrefix + key }

// Get returns the stored value decoded for registered transport key prefixes,
// or as raw JSON bytes when no decoder matches.
//
// Two deliberate choices, both dictated by existing consumers rather than
// preference:
//
// Returning []byte rather than a reconstructed Go value. The interface's `any`
// cannot carry type information across a process boundary, and every in-tree
// consumer already handles the []byte case explicitly, unmarshalling and
// falling back to the raw string — see core/bifrost.go:9336
// (getCachedKeyFromStore), plugins/routing/complexitysession.go:131
// (decodeStoredComplexityTier) and complexitykvwarmcoordinator.go:125
// (decodeStoredWarmGeneration). Upstream wrote those branches for exactly this
// shape: its own gossip path delivers "raw JSON bytes ... unless a decoder is
// registered for its key prefix".
//
// Returning kvstore.ErrNotFound, the framework's sentinel, rather than a local
// one. complexitysession.go:52 tests the miss with
// errors.Is(err, kvstore.ErrNotFound); a different sentinel would turn every
// cache miss into a hard error there and fail the request instead of treating
// the session as new.
func (s *Store) Get(key string) (any, error) {
	if key == "" {
		return nil, kvstore.ErrEmptyKey
	}
	ctx, cancel := s.ctx()
	defer cancel()

	raw, err := s.client.Get(ctx, s.k(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, kvstore.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rediskv: get %q: %w", key, err)
	}
	return s.decodeValue(key, raw), nil
}

// GetAndDelete atomically consumes a value. Redis GETDEL prevents two gateway
// nodes from both accepting the same one-shot transport token or upload state.
func (s *Store) GetAndDelete(key string) (any, error) {
	if key == "" {
		return nil, kvstore.ErrEmptyKey
	}
	ctx, cancel := s.ctx()
	defer cancel()

	raw, err := s.client.GetDel(ctx, s.k(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, kvstore.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rediskv: getdel %q: %w", key, err)
	}
	return s.decodeValue(key, raw), nil
}

// RegisterDecoder restores transport-specific concrete values after Redis has
// carried them across a process boundary. The longest matching prefix wins,
// matching framework/kvstore.Store.
func (s *Store) RegisterDecoder(keyPrefix string, decoder kvstore.TypeDecoder) {
	s.decoderMu.Lock()
	s.decoders[keyPrefix] = decoder
	s.decoderMu.Unlock()
}

func (s *Store) decodeValue(key string, raw []byte) any {
	s.decoderMu.RLock()
	var bestPrefix string
	var bestDecoder kvstore.TypeDecoder
	for prefix, decoder := range s.decoders {
		if strings.HasPrefix(key, prefix) && len(prefix) > len(bestPrefix) {
			bestPrefix = prefix
			bestDecoder = decoder
		}
	}
	s.decoderMu.RUnlock()

	if bestDecoder != nil {
		if value, err := bestDecoder(raw); err == nil {
			return value
		}
	}
	return raw
}

// SetWithTTL stores value as JSON. ttl=0 means no expiration, matching
// framework/kvstore.Store.SetWithTTL.
func (s *Store) SetWithTTL(key string, value any, ttl time.Duration) error {
	if key == "" {
		return kvstore.ErrEmptyKey
	}
	if ttl < 0 {
		return kvstore.ErrInvalidTTL
	}
	payload, err := sonic.Marshal(value)
	if err != nil {
		return fmt.Errorf("rediskv: marshal value for %q: %w", key, err)
	}

	ctx, cancel := s.ctx()
	defer cancel()
	if err := s.client.Set(ctx, s.k(key), payload, ttl).Err(); err != nil {
		return fmt.Errorf("rediskv: set %q: %w", key, err)
	}
	return nil
}

// SetNXWithTTL sets only if the key is absent, reporting whether it was set.
//
// Unlike the in-memory store — whose atomicity is a process-local mutex, so two
// nodes can both win — this maps to Redis SET NX, which is atomic across every
// node sharing the server. That difference is the point of the whole
// deliverable: it is what makes the routing warm claim an actual claim and the
// job sweeper's poll lease an actual lease.
func (s *Store) SetNXWithTTL(key string, value any, ttl time.Duration) (bool, error) {
	if key == "" {
		return false, kvstore.ErrEmptyKey
	}
	if ttl < 0 {
		return false, kvstore.ErrInvalidTTL
	}
	payload, err := sonic.Marshal(value)
	if err != nil {
		return false, fmt.Errorf("rediskv: marshal value for %q: %w", key, err)
	}

	ctx, cancel := s.ctx()
	defer cancel()
	set, err := s.client.SetNX(ctx, s.k(key), payload, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("rediskv: setnx %q: %w", key, err)
	}
	return set, nil
}

// Delete removes the key, reporting whether it existed.
func (s *Store) Delete(key string) (bool, error) {
	if key == "" {
		return false, kvstore.ErrEmptyKey
	}
	ctx, cancel := s.ctx()
	defer cancel()

	removed, err := s.client.Del(ctx, s.k(key)).Result()
	if err != nil {
		return false, fmt.Errorf("rediskv: delete %q: %w", key, err)
	}
	return removed > 0, nil
}

// Ping reports reachability, for health checks.
func (s *Store) Ping() error {
	ctx, cancel := s.ctx()
	defer cancel()
	return s.client.Ping(ctx).Err()
}

// Close releases the client's connections.
func (s *Store) Close() error { return s.client.Close() }
