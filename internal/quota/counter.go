// Package quota provides the shared, post-request usage counter for M3.
//
// This package intentionally implements the intermediate D13 tier: an atomic
// Redis INCRBY after usage is known, with an optional idempotency claim. It is
// not a reservation system and must not be used to advertise a zero-overage
// prepaid hard cap.
package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/redis/go-redis/v9"
)

var (
	ErrRedisRequired = errors.New("quota: redis client is required")
	ErrInvalidKey    = errors.New("quota: tenant and budget are required")
	ErrInvalidAmount = errors.New("quota: amount must be positive")
)

const defaultPrefix = "bifrost:maas:quota:"

var chargeScript = redis.NewScript(`
local existed = redis.call('EXISTS', KEYS[1])
local total = redis.call('INCRBY', KEYS[1], ARGV[1])
if existed == 0 and ARGV[2] ~= '0' then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return total
`)

var chargeOnceScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 1 then
  return {redis.call('GET', KEYS[1]) or '0', 0}
end
local existed = redis.call('EXISTS', KEYS[1])
local total = redis.call('INCRBY', KEYS[1], ARGV[1])
if existed == 0 and ARGV[2] ~= '0' then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
if ARGV[2] ~= '0' then
  redis.call('SET', KEYS[2], '1', 'PX', ARGV[2], 'NX')
else
  redis.call('SET', KEYS[2], '1', 'NX')
end
return {total, 1}
`)

// Counter is safe for concurrent callers and all instances sharing the Redis
// server. The client is injected so the production connection can be shared
// with rediskv without creating a second pool.
type Counter struct {
	client redis.UniversalClient
	prefix string
}

func NewCounter(client redis.UniversalClient, prefix string) (*Counter, error) {
	if client == nil {
		return nil, ErrRedisRequired
	}
	if prefix == "" {
		prefix = defaultPrefix
	}
	return &Counter{client: client, prefix: prefix}, nil
}

func (c *Counter) key(id tenant.ID, budget string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrRedisRequired
	}
	if strings.TrimSpace(string(id)) == "" || strings.TrimSpace(budget) == "" {
		return "", ErrInvalidKey
	}
	// Hash the caller-controlled portions so tenant/budget ids cannot create
	// Redis key separators or unexpectedly collide with internal namespaces.
	h := sha256.Sum256([]byte(string(id) + "\x00" + budget))
	return c.prefix + hex.EncodeToString(h[:]), nil
}

func idempotencyKey(counterKey, requestID string) string {
	h := sha256.Sum256([]byte(requestID))
	return counterKey + ":claim:" + hex.EncodeToString(h[:])
}

func ttlMillis(ttl time.Duration) string {
	if ttl <= 0 {
		return "0"
	}
	ms := ttl.Milliseconds()
	if ms == 0 {
		ms = 1
	}
	return strconv.FormatInt(ms, 10)
}

// Charge atomically adds amount and returns the resulting total. ttl applies
// only when the counter is first created, which preserves a fixed accounting
// window instead of extending it on every request.
func (c *Counter) Charge(ctx context.Context, id tenant.ID, budget string, amount int64, ttl time.Duration) (int64, error) {
	if amount <= 0 {
		return 0, ErrInvalidAmount
	}
	key, err := c.key(id, budget)
	if err != nil {
		return 0, err
	}
	result, err := chargeScript.Run(ctx, c.client, []string{key}, amount, ttlMillis(ttl)).Int64()
	if err != nil {
		return 0, fmt.Errorf("quota: charge %s/%s: %w", id, budget, err)
	}
	return result, nil
}

// ChargeOnce is Charge with an atomic request/attempt claim. Replaying the
// same requestID returns the existing total and applied=false without adding
// usage. Callers should use the same idempotency identity as governance's
// RequestID + AttemptNumber billing claim.
func (c *Counter) ChargeOnce(ctx context.Context, id tenant.ID, budget, requestID string, amount int64, ttl time.Duration) (total int64, applied bool, err error) {
	if amount <= 0 {
		return 0, false, ErrInvalidAmount
	}
	if strings.TrimSpace(requestID) == "" {
		return 0, false, errors.New("quota: request id is required")
	}
	key, err := c.key(id, budget)
	if err != nil {
		return 0, false, err
	}
	result, err := chargeOnceScript.Run(ctx, c.client, []string{key, idempotencyKey(key, requestID)}, amount, ttlMillis(ttl)).Result()
	if err != nil {
		return 0, false, fmt.Errorf("quota: idempotent charge %s/%s: %w", id, budget, err)
	}
	values, ok := result.([]interface{})
	if !ok || len(values) != 2 {
		return 0, false, fmt.Errorf("quota: unexpected charge result %T", result)
	}
	total, err = redisValueInt64(values[0])
	if err != nil {
		return 0, false, err
	}
	flag, err := redisValueInt64(values[1])
	if err != nil {
		return 0, false, err
	}
	return total, flag == 1, nil
}

func redisValueInt64(value interface{}) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case string:
		out, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("quota: invalid redis integer %q: %w", v, err)
		}
		return out, nil
	case []byte:
		out, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("quota: invalid redis integer %q: %w", v, err)
		}
		return out, nil
	default:
		return 0, fmt.Errorf("quota: invalid redis integer type %T", value)
	}
}

func (c *Counter) Usage(ctx context.Context, id tenant.ID, budget string) (int64, error) {
	key, err := c.key(id, budget)
	if err != nil {
		return 0, err
	}
	value, err := c.client.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("quota: usage %s/%s: %w", id, budget, err)
	}
	return value, nil
}

func (c *Counter) Reset(ctx context.Context, id tenant.ID, budget string) error {
	key, err := c.key(id, budget)
	if err != nil {
		return err
	}
	if err := c.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("quota: reset %s/%s: %w", id, budget, err)
	}
	return nil
}
