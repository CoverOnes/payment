// Package events provides event publishing for the payment service.
package events

import (
	"context"

	"github.com/CoverOnes/payment/internal/domain"
)

// Publisher publishes domain events to a transport (Redis pub/sub).
// Implementations must be safe for concurrent use.
type Publisher interface {
	// PublishTransactionStatusChanged sends the payment.transaction_status_changed event.
	// Best-effort: callers MUST NOT treat a publish failure as a reason to
	// roll back the state transition. The transactions row is the authoritative record.
	PublishTransactionStatusChanged(ctx context.Context, evt *domain.TransactionStatusChangedEvent) error
}
