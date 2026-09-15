// Package fairness implements M10's cluster-wide expiring semaphore. A slot
// is a lease, not a process-local counter, so crashed nodes are reclaimed by
// expiry. BYOK requests should bypass this package at the caller.
package fairness

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrRedisRequired  = errors.New("fairness: redis client is required")
	ErrInvalidRequest = errors.New("fairness: invalid semaphore request")
	ErrUnavailable    = errors.New("fairness: semaphore capacity unavailable")
	ErrLeaseExpired   = errors.New("fairness: lease expired")
)

// All keys intentionally use one Redis hash tag. This keeps tenant and
// provider keys in one slot, which is required for an atomic cross-dimension
// Lua decision on Redis Cluster. Deployments with extreme cardinality should
// shard this tag by an explicit semaphore partition, not silently split the
// atomic operation across slots.
const hashTag = "{maas-semaphore}"

var acquireScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local expires = now + tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
if tonumber(ARGV[3]) > 0 and redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[3]) then return 0 end
if tonumber(ARGV[4]) > 0 and redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[4]) then return 0 end
redis.call('ZADD', KEYS[1], expires, ARGV[5])
redis.call('ZADD', KEYS[2], expires, ARGV[5])
redis.call('SET', KEYS[3], ARGV[6], 'PX', ARGV[2])
return 1
`)

var renewScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[3]) == 0 then return 0 end
local expires = tonumber(ARGV[1]) + tonumber(ARGV[2])
redis.call('ZADD', KEYS[1], expires, ARGV[3])
redis.call('ZADD', KEYS[2], expires, ARGV[3])
redis.call('PEXPIRE', KEYS[3], ARGV[2])
return 1
`)

var releaseScript = redis.NewScript(`
redis.call('ZREM', KEYS[1], ARGV[1])
redis.call('ZREM', KEYS[2], ARGV[1])
return redis.call('DEL', KEYS[3])
`)

type Semaphore struct {
	client redis.UniversalClient
	prefix string
}
type Lease struct {
	Token, TenantID, Provider string
	ExpiresAt                 time.Time
}

func NewSemaphore(client redis.UniversalClient, prefix string) (*Semaphore, error) {
	if client == nil {
		return nil, ErrRedisRequired
	}
	if prefix == "" {
		prefix = "bifrost:maas:fairness:"
	}
	return &Semaphore{client: client, prefix: strings.TrimSuffix(prefix, ":") + ":"}, nil
}

func (s *Semaphore) keys(tenantID, provider, token string) []string {
	base := s.prefix + hashTag
	return []string{base + ":tenant:" + keyPart(tenantID), base + ":provider:" + keyPart(provider), base + ":lease:" + keyPart(token)}
}

func keyPart(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (s *Semaphore) Acquire(ctx context.Context, tenantID, provider string, tenantLimit, providerLimit int, ttl time.Duration) (Lease, error) {
	if s == nil || s.client == nil {
		return Lease{}, ErrRedisRequired
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(provider) == "" || tenantLimit < 0 || providerLimit < 0 || ttl <= 0 {
		return Lease{}, ErrInvalidRequest
	}
	token, err := randomToken()
	if err != nil {
		return Lease{}, err
	}
	now := time.Now().UnixMilli()
	value := tenantID + "\x00" + provider
	result, err := acquireScript.Run(ctx, s.client, s.keys(tenantID, provider, token), now, ttl.Milliseconds(), tenantLimit, providerLimit, token, value).Int64()
	if err != nil {
		return Lease{}, fmt.Errorf("fairness: acquire: %w", err)
	}
	if result != 1 {
		return Lease{}, ErrUnavailable
	}
	return Lease{Token: token, TenantID: tenantID, Provider: provider, ExpiresAt: time.UnixMilli(now + ttl.Milliseconds())}, nil
}

func (s *Semaphore) Renew(ctx context.Context, lease Lease, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return ErrRedisRequired
	}
	if lease.Token == "" || lease.TenantID == "" || lease.Provider == "" || ttl <= 0 {
		return ErrInvalidRequest
	}
	now := time.Now().UnixMilli()
	result, err := renewScript.Run(ctx, s.client, s.keys(lease.TenantID, lease.Provider, lease.Token), now, ttl.Milliseconds(), lease.Token).Int64()
	if err != nil {
		return fmt.Errorf("fairness: renew: %w", err)
	}
	if result != 1 {
		return ErrLeaseExpired
	}
	return nil
}

func (s *Semaphore) Release(ctx context.Context, lease Lease) error {
	if s == nil || s.client == nil {
		return ErrRedisRequired
	}
	if lease.Token == "" || lease.TenantID == "" || lease.Provider == "" {
		return ErrInvalidRequest
	}
	if _, err := releaseScript.Run(ctx, s.client, s.keys(lease.TenantID, lease.Provider, lease.Token), lease.Token).Result(); err != nil {
		return fmt.Errorf("fairness: release: %w", err)
	}
	return nil
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ValidateLimits is shared by admission callers that need to reject malformed
// plan data before attempting a Redis operation.
func ValidateLimits(tenantLimit, providerLimit int) error {
	if tenantLimit < 0 || providerLimit < 0 {
		return ErrInvalidRequest
	}
	return nil
}
