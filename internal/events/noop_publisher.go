package events

import (
	"context"

	"github.com/CoverOnes/payment/internal/domain"
)

// NoopPublisher discards all events. Used when Redis is unavailable (dev / no-Redis).
type NoopPublisher struct{}

// NewNoopPublisher returns a NoopPublisher.
func NewNoopPublisher() *NoopPublisher {
	return &NoopPublisher{}
}

// PublishTransactionStatusChanged is a no-op.
func (p *NoopPublisher) PublishTransactionStatusChanged(_ context.Context, _ *domain.TransactionStatusChangedEvent) error {
	return nil
}
