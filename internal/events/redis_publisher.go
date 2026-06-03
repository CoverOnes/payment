package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/redis/go-redis/v9"
)

const channelTransactionStatusChanged = "payment.transaction_status_changed"

// RedisPublisher publishes events to Redis pub/sub channels.
type RedisPublisher struct {
	rdb *redis.Client
}

// NewRedisPublisher returns a RedisPublisher backed by the given Redis client.
func NewRedisPublisher(rdb *redis.Client) *RedisPublisher {
	return &RedisPublisher{rdb: rdb}
}

// PublishTransactionStatusChanged serializes the event and publishes it to Redis.
// Transport failures are returned to the caller (caller should log and continue —
// the transactions row is the durable source of truth).
func (p *RedisPublisher) PublishTransactionStatusChanged(ctx context.Context, evt *domain.TransactionStatusChangedEvent) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal transaction_status_changed event: %w", err)
	}

	if err := p.rdb.Publish(ctx, channelTransactionStatusChanged, payload).Err(); err != nil {
		return fmt.Errorf("redis publish %s: %w", channelTransactionStatusChanged, err)
	}

	return nil
}
