package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/events"
	"github.com/CoverOnes/payment/internal/handler"
	"github.com/CoverOnes/payment/internal/platform/middleware"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/CoverOnes/payment/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// --- stub stores for handler tests ---

type stubTransactionStore struct {
	txs       map[uuid.UUID]*domain.Transaction
	byIdemKey map[string]*domain.Transaction
}

func newStubTransactionStore() *stubTransactionStore {
	return &stubTransactionStore{
		txs:       make(map[uuid.UUID]*domain.Transaction),
		byIdemKey: make(map[string]*domain.Transaction),
	}
}

func (s *stubTransactionStore) Create(_ context.Context, tx *domain.Transaction) error {
	// Composite key: payer_user_id + idempotency_key (mirrors P2 fix).
	compositeKey := tx.PayerUserID.String() + ":" + tx.IdempotencyKey
	if _, exists := s.byIdemKey[compositeKey]; exists {
		return domain.ErrDuplicateKey
	}

	s.txs[tx.ID] = tx
	s.byIdemKey[compositeKey] = tx

	return nil
}

func (s *stubTransactionStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Transaction, error) {
	if tx, ok := s.txs[id]; ok {
		return tx, nil
	}

	return nil, domain.ErrTransactionNotFound
}

func (s *stubTransactionStore) GetByIDForUpdate(_ context.Context, id uuid.UUID) (*domain.Transaction, error) {
	if tx, ok := s.txs[id]; ok {
		return tx, nil
	}

	return nil, domain.ErrTransactionNotFound
}

func (s *stubTransactionStore) GetByIdempotencyKey(_ context.Context, payerUserID uuid.UUID, key string) (*domain.Transaction, error) {
	// Scope lookup by payer: composite key in-memory map.
	compositeKey := payerUserID.String() + ":" + key
	if tx, ok := s.byIdemKey[compositeKey]; ok {
		return tx, nil
	}

	return nil, domain.ErrTransactionNotFound
}

func (s *stubTransactionStore) UpdateStatus(_ context.Context, id uuid.UUID, status domain.Status) error {
	if tx, ok := s.txs[id]; ok {
		tx.Status = status
		tx.UpdatedAt = time.Now().UTC()

		return nil
	}

	return domain.ErrTransactionNotFound
}

func (s *stubTransactionStore) ListByUserID(_ context.Context, userID uuid.UUID) ([]*domain.Transaction, error) {
	var result []*domain.Transaction

	for _, tx := range s.txs {
		if tx.PayerUserID == userID || tx.PayeeUserID == userID {
			result = append(result, tx)
		}
	}

	return result, nil
}

type stubAuditStore struct{}

func (s *stubAuditStore) Append(_ context.Context, _ *domain.TransactionAudit) error {
	return nil
}

type stubTxManager struct {
	txStore    store.TransactionStore
	auditStore store.AuditStore
}

func (m *stubTxManager) WithTx(ctx context.Context, fn func(ctx context.Context, txs store.TransactionStore, audits store.AuditStore) error) error {
	return fn(ctx, m.txStore, m.auditStore)
}

func buildRouter(txStore *stubTransactionStore) *gin.Engine {
	auStore := &stubAuditStore{}
	txMgr := &stubTxManager{txStore: txStore, auditStore: auStore}
	publisher := events.NewNoopPublisher()
	txSvc := service.NewTransactionService(txStore, auStore, txMgr, publisher)
	txH := handler.NewTransactionHandler(txSvc)

	r := gin.New()
	r.Use(middleware.RequireValidIdentity())

	// Mirror router config — tier3 gate on mutations, no gate on reads.
	api := r.Group("/v1")
	api.POST("/transactions", middleware.RequireTier(3), txH.Create)
	api.POST("/transactions/:id/release", middleware.RequireTier(3), txH.Release)
	api.POST("/transactions/:id/refund", middleware.RequireTier(3), txH.Refund)
	api.GET("/transactions/:id", txH.GetByID)
	api.GET("/me/transactions", txH.ListMyTransactions)

	return r
}

// TestTransactionHandler_TierGate verifies that RequireTier(3) returns 403 KYC_TIER_REQUIRED
// when the caller has a lower tier, for all money mutation endpoints.
func TestTransactionHandler_TierGate(t *testing.T) {
	t.Parallel()

	payee := uuid.New()

	tests := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{
			name:   "POST /v1/transactions tier<3 -> 403",
			method: http.MethodPost,
			path:   "/v1/transactions",
			body: map[string]any{
				"payeeUserId": payee.String(),
				"amount":      "100.00",
				"currency":    "TWD",
			},
		},
		{
			name:   "POST /v1/transactions/:id/release tier<3 -> 403",
			method: http.MethodPost,
			path:   "/v1/transactions/" + uuid.New().String() + "/release",
			body:   nil,
		},
		{
			name:   "POST /v1/transactions/:id/refund tier<3 -> 403",
			method: http.MethodPost,
			path:   "/v1/transactions/" + uuid.New().String() + "/refund",
			body:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := buildRouter(newStubTransactionStore())
			userID := uuid.New()

			var bodyBuf *bytes.Reader
			if tc.body != nil {
				b, _ := json.Marshal(tc.body)
				bodyBuf = bytes.NewReader(b)
			} else {
				bodyBuf = bytes.NewReader(nil)
			}

			req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, bodyBuf)
			req.Header.Set("X-User-Id", userID.String())
			req.Header.Set("X-Kyc-Tier", "2") // below required tier 3
			req.Header.Set("Idempotency-Key", uuid.New().String())
			req.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, http.StatusForbidden, w.Code)

			var resp struct {
				Error struct {
					Code    string `json:"code"`
					Details struct {
						RequiredTier int `json:"requiredTier"`
						CurrentTier  int `json:"currentTier"`
					} `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
			assert.Equal(t, "KYC_TIER_REQUIRED", resp.Error.Code)
			assert.Equal(t, 3, resp.Error.Details.RequiredTier)
			assert.Equal(t, 2, resp.Error.Details.CurrentTier)
		})
	}
}

// TestTransactionHandler_IDOR verifies that GET /v1/transactions/:id returns 404
// when the caller is neither payer nor payee (no existence leak).
func TestTransactionHandler_IDOR(t *testing.T) {
	t.Parallel()

	txStore := newStubTransactionStore()
	r := buildRouter(txStore)

	payer := uuid.New()
	payee := uuid.New()
	attacker := uuid.New()

	// Seed a transaction.
	now := time.Now().UTC()
	amount500, _ := decimal.NewFromString("500.00") // test fixture literal — always valid

	tx := &domain.Transaction{
		ID:             uuid.New(),
		PayerUserID:    payer,
		PayeeUserID:    payee,
		Amount:         amount500,
		Currency:       "TWD",
		Status:         domain.StatusHeld,
		IdempotencyKey: "test-idor-key",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	txStore.txs[tx.ID] = tx
	txStore.byIdemKey[tx.PayerUserID.String()+":"+tx.IdempotencyKey] = tx

	t.Run("payer can fetch own transaction", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/transactions/"+tx.ID.String(), http.NoBody)
		req.Header.Set("X-User-Id", payer.String())
		req.Header.Set("X-Kyc-Tier", "3")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("payee can fetch own transaction", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/transactions/"+tx.ID.String(), http.NoBody)
		req.Header.Set("X-User-Id", payee.String())
		req.Header.Set("X-Kyc-Tier", "3")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("IDOR: third party gets 404 not 403", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/transactions/"+tx.ID.String(), http.NoBody)
		req.Header.Set("X-User-Id", attacker.String())
		req.Header.Set("X-Kyc-Tier", "3")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var resp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		assert.Equal(t, "TRANSACTION_NOT_FOUND", resp.Error.Code)
	})
}

// seedHeldTx seeds a HELD transaction in a fresh stub store and returns the tx and router.
// The idemKey must be unique per call to avoid composite-key collisions across parallel sub-tests.
func seedHeldTx(t *testing.T, payer, payee uuid.UUID, idemKey string) (*domain.Transaction, *gin.Engine) {
	t.Helper()

	now := time.Now().UTC()
	amount500, _ := decimal.NewFromString("500.00") // test fixture literal — always valid

	freshStore := newStubTransactionStore()
	tx := &domain.Transaction{
		ID:             uuid.New(),
		PayerUserID:    payer,
		PayeeUserID:    payee,
		Amount:         amount500,
		Currency:       "TWD",
		Status:         domain.StatusHeld,
		IdempotencyKey: idemKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	freshStore.txs[tx.ID] = tx
	freshStore.byIdemKey[tx.PayerUserID.String()+":"+tx.IdempotencyKey] = tx

	return tx, buildRouter(freshStore)
}

// TestTransactionHandler_Release_IDOR and TestTransactionHandler_Refund_IDOR are merged
// into a single table-driven test to cover both transition endpoints without code duplication.
//
// Covered cases per endpoint:
//   - third party (not payer, not payee) → 404 TRANSACTION_NOT_FOUND (no existence leak)
//   - payer → 200 OK
//   - payee → 200 OK
func TestTransactionHandler_Transition_IDOR(t *testing.T) {
	t.Parallel()

	payer := uuid.New()
	payee := uuid.New()
	attacker := uuid.New()

	endpoints := []struct {
		name     string
		action   string
		idemBase string
	}{
		{"release", "release", "rls"},
		{"refund", "refund", "rfd"},
	}

	for _, ep := range endpoints {
		t.Run(ep.name+"/IDOR third party gets 404", func(t *testing.T) {
			t.Parallel()

			tx, r := seedHeldTx(t, payer, payee, ep.idemBase+"-idor")
			path := "/v1/transactions/" + tx.ID.String() + "/" + ep.action

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, http.NoBody)
			req.Header.Set("X-User-Id", attacker.String())
			req.Header.Set("X-Kyc-Tier", "3")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, http.StatusNotFound, w.Code)

			var resp struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
			assert.Equal(t, "TRANSACTION_NOT_FOUND", resp.Error.Code)
		})

		t.Run(ep.name+"/payer succeeds", func(t *testing.T) {
			t.Parallel()

			tx, r := seedHeldTx(t, payer, payee, ep.idemBase+"-payer")

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/transactions/"+tx.ID.String()+"/"+ep.action, http.NoBody)
			req.Header.Set("X-User-Id", payer.String())
			req.Header.Set("X-Kyc-Tier", "3")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code)
		})

		t.Run(ep.name+"/payee succeeds", func(t *testing.T) {
			t.Parallel()

			tx, r := seedHeldTx(t, payer, payee, ep.idemBase+"-payee")

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/transactions/"+tx.ID.String()+"/"+ep.action, http.NoBody)
			req.Header.Set("X-User-Id", payee.String())
			req.Header.Set("X-Kyc-Tier", "3")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code)
		})
	}
}

// TestTransactionHandler_MissingIdentity verifies that missing X-User-Id returns 401.
func TestTransactionHandler_MissingIdentity(t *testing.T) {
	t.Parallel()

	r := buildRouter(newStubTransactionStore())

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/me/transactions", http.NoBody)
	// No X-User-Id header.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestTransactionHandler_Create_Idempotency verifies that a duplicate Idempotency-Key
// returns the existing transaction (HTTP 201) rather than an error.
func TestTransactionHandler_Create_Idempotency(t *testing.T) {
	t.Parallel()

	txStore := newStubTransactionStore()
	r := buildRouter(txStore)

	payer := uuid.New()
	payee := uuid.New()
	key := uuid.New().String()

	body := map[string]any{
		"payeeUserId": payee.String(),
		"amount":      "200.00",
		"currency":    "TWD",
	}

	b, _ := json.Marshal(body)

	makeReq := func() *http.Request {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/transactions", bytes.NewReader(b))
		req.Header.Set("X-User-Id", payer.String())
		req.Header.Set("X-Kyc-Tier", "3")
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("Content-Type", "application/json")

		return req
	}

	// First request — creates transaction.
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, makeReq())
	assert.Equal(t, http.StatusCreated, w1.Code)

	// Second request with same key — returns existing.
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, makeReq())
	assert.Equal(t, http.StatusCreated, w2.Code)

	var resp1, resp2 struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(w1.Body).Decode(&resp1))
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&resp2))
	assert.Equal(t, resp1.Data.ID, resp2.Data.ID, "idempotent: same transaction returned")
}
