package configbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/redis/go-redis/v9"
)

const DefaultChannel = "bifrost:maas:config-changes"

// Notification is the only payload sent over the acceleration channel. It is
// intentionally not a snapshot or an outbox id: a lost message is harmless
// because periodic generation reconciliation remains authoritative.
type Notification struct {
	TenantID   tenant.ID `json:"tenant_id"`
	Generation uint64    `json:"generation"`
}

type RedisNotifier struct {
	client  redis.UniversalClient
	channel string
}

func NewRedisNotifier(client redis.UniversalClient, channel string) (*RedisNotifier, error) {
	if client == nil {
		return nil, errors.New("configbus: redis client is required")
	}
	if channel == "" {
		channel = DefaultChannel
	}
	return &RedisNotifier{client: client, channel: channel}, nil
}

func (n *RedisNotifier) Publish(ctx context.Context, change Change) error {
	if n == nil || n.client == nil {
		return errors.New("configbus: redis notifier is nil")
	}
	payload, err := json.Marshal(Notification{TenantID: change.TenantID, Generation: change.Generation})
	if err != nil {
		return fmt.Errorf("configbus: encode notification: %w", err)
	}
	if err := n.client.Publish(ctx, n.channel, payload).Err(); err != nil {
		return fmt.Errorf("configbus: publish notification: %w", err)
	}
	return nil
}

// Subscribe starts a cancellable receiver. Malformed messages are reported on
// the error channel and do not terminate the subscription.
func (n *RedisNotifier) Subscribe(ctx context.Context) (<-chan Notification, <-chan error, error) {
	if n == nil || n.client == nil {
		return nil, nil, errors.New("configbus: redis notifier is nil")
	}
	pubsub := n.client.Subscribe(ctx, n.channel)
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, nil, fmt.Errorf("configbus: subscribe notification: %w", err)
	}
	out := make(chan Notification)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		defer pubsub.Close()
		for {
			msg, err := pubsub.ReceiveMessage(ctx)
			if err != nil {
				if ctx.Err() == nil {
					errs <- fmt.Errorf("configbus: receive notification: %w", err)
				}
				return
			}
			var notification Notification
			if err := json.Unmarshal([]byte(msg.Payload), &notification); err != nil || notification.TenantID == "" || notification.Generation == 0 {
				if err == nil {
					err = errors.New("tenant_id and generation are required")
				}
				select {
				case errs <- fmt.Errorf("configbus: invalid notification: %w", err):
				default:
				}
				continue
			}
			select {
			case out <- notification:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, errs, nil
}
