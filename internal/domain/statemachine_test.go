package domain_test

import (
	"testing"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		from  domain.Status
		to    domain.Status
		valid bool
	}{
		// Valid transitions
		{"PENDING->HELD", domain.StatusPending, domain.StatusHeld, true},
		{"PENDING->FAILED", domain.StatusPending, domain.StatusFailed, true},
		{"HELD->RELEASED", domain.StatusHeld, domain.StatusReleased, true},
		{"HELD->REFUNDED", domain.StatusHeld, domain.StatusRefunded, true},
		{"HELD->FAILED", domain.StatusHeld, domain.StatusFailed, true},

		// Invalid transitions — terminal states have no outgoing edges
		{"RELEASED->anything", domain.StatusReleased, domain.StatusFailed, false},
		{"REFUNDED->anything", domain.StatusRefunded, domain.StatusFailed, false},
		{"FAILED->anything", domain.StatusFailed, domain.StatusPending, false},

		// Invalid transitions — wrong direction
		{"HELD->PENDING", domain.StatusHeld, domain.StatusPending, false},
		{"RELEASED->PENDING", domain.StatusReleased, domain.StatusPending, false},

		// Self-transition always invalid
		{"PENDING->PENDING", domain.StatusPending, domain.StatusPending, false},
		{"HELD->HELD", domain.StatusHeld, domain.StatusHeld, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := domain.CanTransition(tc.from, tc.to)
			assert.Equal(t, tc.valid, got)
		})
	}
}

func TestTransition_Valid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		from    domain.Status
		to      domain.Status
		wantErr bool
	}{
		{"PENDING->HELD ok", domain.StatusPending, domain.StatusHeld, false},
		{"HELD->RELEASED ok", domain.StatusHeld, domain.StatusReleased, false},
		{"HELD->REFUNDED ok", domain.StatusHeld, domain.StatusRefunded, false},
		{"HELD->FAILED ok", domain.StatusHeld, domain.StatusFailed, false},
		{"PENDING->FAILED ok", domain.StatusPending, domain.StatusFailed, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := domain.Transition(tc.from, tc.to)
			require.NoError(t, err)
			assert.Equal(t, tc.to, got)
		})
	}
}

func TestTransition_Invalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from domain.Status
		to   domain.Status
	}{
		{"RELEASED is terminal", domain.StatusReleased, domain.StatusRefunded},
		{"REFUNDED is terminal", domain.StatusRefunded, domain.StatusHeld},
		{"FAILED is terminal", domain.StatusFailed, domain.StatusPending},
		{"HELD cannot go PENDING", domain.StatusHeld, domain.StatusPending},
		{"unknown status", domain.Status("UNKNOWN"), domain.StatusHeld},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := domain.Transition(tc.from, tc.to)
			require.ErrorIs(t, err, domain.ErrInvalidTransition)
		})
	}
}
