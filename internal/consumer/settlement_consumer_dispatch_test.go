package consumer

// White-box tests for SettlementConsumer handler dispatch.
// These tests live in package consumer (not consumer_test) so they can access
// unexported methods handleContractActivated and handleContractCompleted directly.
// They inject a stubSettlementService via newSettlementConsumerWithService.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── Stub service ─────────────────────────────────────────────────────────────

// stubSettlementService is a test double for settlementServicer that records calls.
type stubSettlementService struct {
	// createPlanResult: result/err returned by CreatePlan
	createPlanResult *domain.SettlementPlan
	createPlanErr    error
	createPlanCalls  atomic.Int64

	// getPlanResult: result/err returned by GetPlanByContractID
	getPlanResult *domain.SettlementPlan
	getPlanErr    error

	// disburseMilestoneErr: error returned by DisburseMilestone (nil = success)
	disburseMilestoneErr    error
	disburseMilestoneCalls  atomic.Int64
	disburseMilestoneLastIn *service.DisburseMilestoneInput
}

func (s *stubSettlementService) CreatePlan(
	_ context.Context,
	_ *service.CreatePlanInput,
) (*domain.SettlementPlan, error) {
	s.createPlanCalls.Add(1)
	return s.createPlanResult, s.createPlanErr
}

func (s *stubSettlementService) GetPlanByContractID(
	_ context.Context,
	_ uuid.UUID,
) (*domain.SettlementPlan, error) {
	return s.getPlanResult, s.getPlanErr
}

func (s *stubSettlementService) DisburseMilestone(
	_ context.Context,
	in *service.DisburseMilestoneInput,
) (*service.DisburseResult, error) {
	s.disburseMilestoneCalls.Add(1)
	s.disburseMilestoneLastIn = in

	if s.disburseMilestoneErr != nil {
		return nil, s.disburseMilestoneErr
	}

	return &service.DisburseResult{DisbursedCount: 1}, nil
}

// ─── contractActivatedEvent helpers ──────────────────────────────────────────

type testContractActivatedData struct {
	ContractID uuid.UUID `json:"contractId"`
	TenderID   uuid.UUID `json:"tenderId"`
	PartyCount int       `json:"partyCount"`
}

type testContractActivatedEvent struct {
	EventID    uuid.UUID                 `json:"eventId"`
	OccurredAt time.Time                 `json:"occurredAt"`
	Version    int                       `json:"version"`
	Data       testContractActivatedData `json:"data"`
}

func mustMarshalActivated(t *testing.T, contractID, tenderID uuid.UUID) string {
	t.Helper()

	evt := testContractActivatedEvent{
		EventID:    uuid.New(),
		OccurredAt: time.Now(),
		Version:    1,
		Data: testContractActivatedData{
			ContractID: contractID,
			TenderID:   tenderID,
			PartyCount: 2,
		},
	}

	b, err := json.Marshal(evt)
	require.NoError(t, err)

	return string(b)
}

// ─── contractCompletedEvent helpers ──────────────────────────────────────────

type testContractCompletedData struct {
	ContractID  uuid.UUID       `json:"contractId"`
	TenderID    uuid.UUID       `json:"tenderId"`
	MilestoneID uuid.UUID       `json:"milestoneId"`
	Amount      decimal.Decimal `json:"amount"`
	Currency    string          `json:"currency"`
}

type testContractCompletedEvent struct {
	EventID    uuid.UUID                 `json:"eventId"`
	OccurredAt time.Time                 `json:"occurredAt"`
	Version    int                       `json:"version"`
	Data       testContractCompletedData `json:"data"`
}

func mustMarshalCompleted(t *testing.T, contractID, milestoneID uuid.UUID, amount decimal.Decimal) string {
	t.Helper()

	evt := testContractCompletedEvent{
		EventID:    uuid.New(),
		OccurredAt: time.Now(),
		Version:    1,
		Data: testContractCompletedData{
			ContractID:  contractID,
			TenderID:    uuid.New(),
			MilestoneID: milestoneID,
			Amount:      amount,
			Currency:    "TWD",
		},
	}

	b, err := json.Marshal(evt)
	require.NoError(t, err)

	return string(b)
}

// ─── handleContractActivated tests ───────────────────────────────────────────

// TestHandleContractActivated_HappyPath verifies that a well-formed
// contract_activated event triggers CreatePlan exactly once.
func TestHandleContractActivated_HappyPath(t *testing.T) {
	plan := &domain.SettlementPlan{
		ID:     uuid.New(),
		Status: domain.PlanStatusActive,
	}
	stub := &stubSettlementService{createPlanResult: plan}
	c := newSettlementConsumerWithService(stub)

	payload := mustMarshalActivated(t, uuid.New(), uuid.New())
	c.handleContractActivated(context.Background(), payload)

	assert.Equal(t, int64(1), stub.createPlanCalls.Load(),
		"handleContractActivated must call CreatePlan exactly once")
}

// TestHandleContractActivated_IdempotentSkip verifies that when CreatePlan returns
// (nil, nil) — the idempotent-already-exists case — the handler does not panic and
// does not call CreatePlan again.
func TestHandleContractActivated_IdempotentSkip(t *testing.T) {
	stub := &stubSettlementService{createPlanResult: nil, createPlanErr: nil}
	c := newSettlementConsumerWithService(stub)

	payload := mustMarshalActivated(t, uuid.New(), uuid.New())
	c.handleContractActivated(context.Background(), payload)

	assert.Equal(t, int64(1), stub.createPlanCalls.Load(),
		"idempotent skip: CreatePlan called once, returned nil plan — handler must not panic")
}

// TestHandleContractActivated_ServiceError verifies that when CreatePlan returns an
// error the handler logs and returns cleanly (no panic, no further service calls).
func TestHandleContractActivated_ServiceError(t *testing.T) {
	stub := &stubSettlementService{
		createPlanErr: errors.New("db unavailable"),
	}
	c := newSettlementConsumerWithService(stub)

	payload := mustMarshalActivated(t, uuid.New(), uuid.New())

	// Must not panic.
	assert.NotPanics(t, func() {
		c.handleContractActivated(context.Background(), payload)
	}, "service error must not cause panic")

	assert.Equal(t, int64(1), stub.createPlanCalls.Load(),
		"CreatePlan must have been attempted once even on error")
}

// TestHandleContractActivated_MalformedPayload verifies that a malformed JSON payload
// is handled without panic and without calling the service.
func TestHandleContractActivated_MalformedPayload(t *testing.T) {
	stub := &stubSettlementService{}
	c := newSettlementConsumerWithService(stub)

	assert.NotPanics(t, func() {
		c.handleContractActivated(context.Background(), `{not valid json}`)
	}, "malformed JSON must not panic")

	assert.Equal(t, int64(0), stub.createPlanCalls.Load(),
		"malformed payload must not reach CreatePlan")
}

// ─── handleContractCompleted tests ───────────────────────────────────────────

// TestHandleContractCompleted_HappyPath verifies that a well-formed
// contract_completed event calls DisburseMilestone with the correct plan ID,
// milestoneID, and amount.
func TestHandleContractCompleted_HappyPath(t *testing.T) {
	planID := uuid.New()
	contractID := uuid.New()
	milestoneID := uuid.New()
	amount, _ := decimal.NewFromString("2500.00")

	stub := &stubSettlementService{
		getPlanResult: &domain.SettlementPlan{
			ID:     planID,
			Status: domain.PlanStatusActive,
		},
	}
	c := newSettlementConsumerWithService(stub)

	payload := mustMarshalCompleted(t, contractID, milestoneID, amount)
	c.handleContractCompleted(context.Background(), payload)

	assert.Equal(t, int64(1), stub.disburseMilestoneCalls.Load(),
		"DisburseMilestone must be called exactly once")
	require.NotNil(t, stub.disburseMilestoneLastIn,
		"disburseMilestoneLastIn must have been captured")
	assert.Equal(t, planID, stub.disburseMilestoneLastIn.PlanID,
		"DisburseMilestone must receive the correct plan ID")
	assert.Equal(t, milestoneID, stub.disburseMilestoneLastIn.MilestoneID,
		"DisburseMilestone must receive the correct milestone ID")
	assert.True(t, stub.disburseMilestoneLastIn.Amount.Equal(amount),
		"DisburseMilestone must receive the correct amount")
}

// TestHandleContractCompleted_NoPlanFound verifies that when GetPlanByContractID
// returns nil (no plan for contract), DisburseMilestone is never called.
func TestHandleContractCompleted_NoPlanFound(t *testing.T) {
	stub := &stubSettlementService{
		getPlanResult: nil,
		getPlanErr:    nil,
	}
	c := newSettlementConsumerWithService(stub)

	payload := mustMarshalCompleted(t, uuid.New(), uuid.New(), decimal.NewFromInt(1000))
	c.handleContractCompleted(context.Background(), payload)

	assert.Equal(t, int64(0), stub.disburseMilestoneCalls.Load(),
		"DisburseMilestone must not be called when no plan is found")
}

// TestHandleContractCompleted_DisburseFailure verifies that when DisburseMilestone
// returns an error the handler does not panic and the error is logged (no retry —
// pub/sub is fire-and-forget; this pins the "permanently lost" semantics).
func TestHandleContractCompleted_DisburseFailure(t *testing.T) {
	planID := uuid.New()
	stub := &stubSettlementService{
		getPlanResult: &domain.SettlementPlan{
			ID:     planID,
			Status: domain.PlanStatusActive,
		},
		disburseMilestoneErr: errors.New("db timeout"),
	}
	c := newSettlementConsumerWithService(stub)

	payload := mustMarshalCompleted(t, uuid.New(), uuid.New(), decimal.NewFromInt(500))

	assert.NotPanics(t, func() {
		c.handleContractCompleted(context.Background(), payload)
	}, "DisburseMilestone error must not cause panic")

	assert.Equal(t, int64(1), stub.disburseMilestoneCalls.Load(),
		"DisburseMilestone must have been attempted once even on error")
}

// TestHandleContractCompleted_MalformedPayload verifies that a malformed JSON
// payload is handled without panic and without any service calls.
func TestHandleContractCompleted_MalformedPayload(t *testing.T) {
	stub := &stubSettlementService{}
	c := newSettlementConsumerWithService(stub)

	assert.NotPanics(t, func() {
		c.handleContractCompleted(context.Background(), `not json at all`)
	}, "malformed JSON must not panic")

	assert.Equal(t, int64(0), stub.disburseMilestoneCalls.Load(),
		"malformed payload must not reach DisburseMilestone")
}
