package postgres_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CoverOnes/payment/internal/domain"
	"github.com/CoverOnes/payment/internal/store"
	"github.com/CoverOnes/payment/internal/store/postgres"
	migrations "github.com/CoverOnes/payment/migrations"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Type aliases to make WithTx callback signatures readable.
type (
	storeTransactionStore = store.TransactionStore
	storeAuditStore       = store.AuditStore
)

// startTestDB spins up a real Postgres container via testcontainers.
func startTestDB(t *testing.T) string {
	t.Helper()

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	return dsn
}

// runMigrations applies embedded *.up.sql files against the test DB.
func runMigrations(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	var upFiles []string

	err = fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	require.NoError(t, err, "walk embedded migrations FS")
	require.NotEmpty(t, upFiles, "no *.up.sql files found")

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		require.NoError(t, readErr, "read migration file %s", file)

		_, execErr := pool.Exec(ctx, string(data))
		require.NoError(t, execErr, fmt.Sprintf("apply migration %s", file))
	}
}

// newTestTransaction returns a valid Transaction for testing.
func newTestTransaction(payer, payee uuid.UUID, key string) *domain.Transaction {
	now := time.Now().UTC()
	amount, _ := decimal.NewFromString("1000.00") // test fixture literal — always valid

	return &domain.Transaction{
		ID:             uuid.New(),
		PayerUserID:    payer,
		PayeeUserID:    payee,
		Amount:         amount,
		Currency:       "TWD",
		Status:         domain.StatusHeld,
		IdempotencyKey: key,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func TestTransactionStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	txStore := postgres.NewTransactionStore(pool)

	t.Run("Create and GetByID round-trip", func(t *testing.T) {
		payer := uuid.New()
		payee := uuid.New()
		tx := newTestTransaction(payer, payee, uuid.New().String())

		require.NoError(t, txStore.Create(ctx, tx))

		got, err := txStore.GetByID(ctx, tx.ID)
		require.NoError(t, err)
		assert.Equal(t, tx.ID, got.ID)
		assert.Equal(t, payer, got.PayerUserID)
		assert.Equal(t, payee, got.PayeeUserID)
		assert.Equal(t, domain.StatusHeld, got.Status)
		assert.True(t, tx.Amount.Equal(got.Amount), "amount round-trip preserved")
	})

	t.Run("Create: idempotent — duplicate key returns ErrDuplicateKey", func(t *testing.T) {
		payer := uuid.New()
		payee := uuid.New()
		key := uuid.New().String()
		tx := newTestTransaction(payer, payee, key)

		require.NoError(t, txStore.Create(ctx, tx))

		// Second create with same idempotency key.
		tx2 := newTestTransaction(payer, payee, key)
		err := txStore.Create(ctx, tx2)
		require.ErrorIs(t, err, domain.ErrDuplicateKey)
	})

	t.Run("GetByID: ErrTransactionNotFound for missing", func(t *testing.T) {
		_, err := txStore.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrTransactionNotFound)
	})

	t.Run("GetByIdempotencyKey: returns existing transaction (scoped by payer)", func(t *testing.T) {
		payer := uuid.New()
		payee := uuid.New()
		key := uuid.New().String()
		tx := newTestTransaction(payer, payee, key)
		require.NoError(t, txStore.Create(ctx, tx))

		got, err := txStore.GetByIdempotencyKey(ctx, payer, key)
		require.NoError(t, err)
		assert.Equal(t, tx.ID, got.ID)
	})

	t.Run("GetByIdempotencyKey: ErrTransactionNotFound for missing key", func(t *testing.T) {
		payer := uuid.New()
		_, err := txStore.GetByIdempotencyKey(ctx, payer, "nonexistent-key-xyz")
		require.ErrorIs(t, err, domain.ErrTransactionNotFound)
	})

	t.Run("GetByIdempotencyKey: same key for different payer returns not-found (cross-user isolation)", func(t *testing.T) {
		payer1 := uuid.New()
		payer2 := uuid.New()
		payee := uuid.New()
		key := uuid.New().String()

		// payer1 creates a tx with this idempotency key.
		tx1 := newTestTransaction(payer1, payee, key)
		require.NoError(t, txStore.Create(ctx, tx1))

		// payer2 looking up the same key string gets not-found (no leak).
		_, err := txStore.GetByIdempotencyKey(ctx, payer2, key)
		require.ErrorIs(t, err, domain.ErrTransactionNotFound, "different payer must not see another payer's tx")
	})

	t.Run("GetByIdempotencyKey: same key for same payer returns same tx (idempotent)", func(t *testing.T) {
		payer := uuid.New()
		payee := uuid.New()
		key := uuid.New().String()

		tx := newTestTransaction(payer, payee, key)
		require.NoError(t, txStore.Create(ctx, tx))

		// Re-lookup by same payer returns the same transaction.
		got, err := txStore.GetByIdempotencyKey(ctx, payer, key)
		require.NoError(t, err)
		assert.Equal(t, tx.ID, got.ID)
	})

	t.Run("Create: same idempotency key for different payers creates two distinct transactions", func(t *testing.T) {
		payer1 := uuid.New()
		payer2 := uuid.New()
		payee := uuid.New()
		key := uuid.New().String()

		tx1 := newTestTransaction(payer1, payee, key)
		require.NoError(t, txStore.Create(ctx, tx1), "payer1 with key should succeed")

		tx2 := newTestTransaction(payer2, payee, key)
		require.NoError(t, txStore.Create(ctx, tx2), "payer2 with same key string should also succeed — different payer")

		assert.NotEqual(t, tx1.ID, tx2.ID, "two distinct transactions created")

		got1, err := txStore.GetByIdempotencyKey(ctx, payer1, key)
		require.NoError(t, err)
		assert.Equal(t, tx1.ID, got1.ID)

		got2, err := txStore.GetByIdempotencyKey(ctx, payer2, key)
		require.NoError(t, err)
		assert.Equal(t, tx2.ID, got2.ID)
	})
}

// TestTransactionStore_ForUpdateTransition verifies SELECT FOR UPDATE + state transition
// in a single transaction, with audit row written atomically.
func TestTransactionStore_ForUpdateTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	txStore := postgres.NewTransactionStore(pool)
	txMgr := postgres.NewTxManager(pool)

	payer := uuid.New()
	payee := uuid.New()
	now := time.Now().UTC()

	amount500, _ := decimal.NewFromString("500.00") // test fixture literal — always valid

	original := &domain.Transaction{
		ID:             uuid.New(),
		PayerUserID:    payer,
		PayeeUserID:    payee,
		Amount:         amount500,
		Currency:       "TWD",
		Status:         domain.StatusHeld,
		IdempotencyKey: uuid.New().String(),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	require.NoError(t, txStore.Create(ctx, original))

	t.Run("HELD->RELEASED in a transaction with audit row", func(t *testing.T) {
		// Import the store package for the WithTx signature.
		err := txMgr.WithTx(ctx, func(ctx context.Context, txs storeTransactionStore, audits storeAuditStore) error {
			current, lockErr := txs.GetByIDForUpdate(ctx, original.ID)
			require.NoError(t, lockErr)
			assert.Equal(t, domain.StatusHeld, current.Status)

			// Validate transition.
			_, validErr := domain.Transition(current.Status, domain.StatusReleased)
			require.NoError(t, validErr)

			updateErr := txs.UpdateStatus(ctx, original.ID, domain.StatusReleased)
			require.NoError(t, updateErr)

			audit := &domain.TransactionAudit{
				ID:            uuid.New(),
				TransactionID: original.ID,
				FromStatus:    domain.StatusHeld,
				ToStatus:      domain.StatusReleased,
				ActorUserID:   payer,
				OccurredAt:    time.Now().UTC(),
			}
			auditErr := audits.Append(ctx, audit)
			require.NoError(t, auditErr)

			return nil
		})
		require.NoError(t, err)

		// Verify the status was committed.
		updated, getErr := txStore.GetByID(ctx, original.ID)
		require.NoError(t, getErr)
		assert.Equal(t, domain.StatusReleased, updated.Status)
	})
}

// TestAuditStore_Integration verifies append-only audit log behavior.
func TestAuditStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	txStore := postgres.NewTransactionStore(pool)
	auditStore := postgres.NewAuditStore(pool)

	// Seed a transaction to reference.
	payer := uuid.New()
	payee := uuid.New()
	tx := newTestTransaction(payer, payee, uuid.New().String())
	require.NoError(t, txStore.Create(ctx, tx))

	t.Run("Append: inserts a new audit entry", func(t *testing.T) {
		entry := &domain.TransactionAudit{
			ID:            uuid.New(),
			TransactionID: tx.ID,
			FromStatus:    domain.StatusPending,
			ToStatus:      domain.StatusHeld,
			ActorUserID:   payer,
			OccurredAt:    time.Now().UTC(),
		}

		require.NoError(t, auditStore.Append(ctx, entry))
	})

	t.Run("Append: multiple audit entries for same transaction", func(t *testing.T) {
		entry1 := &domain.TransactionAudit{
			ID:            uuid.New(),
			TransactionID: tx.ID,
			FromStatus:    domain.StatusHeld,
			ToStatus:      domain.StatusReleased,
			ActorUserID:   payee,
			OccurredAt:    time.Now().UTC(),
		}

		entry2 := &domain.TransactionAudit{
			ID:            uuid.New(),
			TransactionID: tx.ID,
			FromStatus:    domain.StatusHeld,
			ToStatus:      domain.StatusRefunded,
			ActorUserID:   payer,
			OccurredAt:    time.Now().UTC().Add(time.Second),
		}

		require.NoError(t, auditStore.Append(ctx, entry1))
		require.NoError(t, auditStore.Append(ctx, entry2))
	})
}

// TestAuditStore_PartitionRouting verifies that transaction_audit PARTITION BY RANGE(occurred_at)
// correctly routes rows into child partitions spanning the current month, an adjacent prior month,
// and an adjacent future month, and that all inserted rows are queryable via the parent table.
//
// This test also confirms that the FOR-UPDATE state-transition path (TestTransactionStore_ForUpdateTransition)
// writes its audit row into the partitioned table without error — the ForUpdate test itself
// already verifies this; here we explicitly confirm multi-partition queries work end-to-end.
func TestAuditStore_PartitionRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	txStore := postgres.NewTransactionStore(pool)
	auditStore := postgres.NewAuditStore(pool)

	// Seed a base transaction to attach all audit rows to.
	payer := uuid.New()
	payee := uuid.New()
	tx := newTestTransaction(payer, payee, uuid.New().String())
	require.NoError(t, txStore.Create(ctx, tx))

	now := time.Now().UTC()

	// Three occurred_at timestamps that span three different calendar months:
	//   - prior month  (now - 35 days): should land in the prior-month partition
	//   - current time (now):           should land in the current-month partition
	//   - future month (now + 40 days): should land in a future-month partition
	//
	// The DO $$ bootstrap in the migration creates partitions for
	// [month-1, month+3] so all three are within an explicit named partition.
	timestamps := []struct {
		name       string
		occurredAt time.Time
	}{
		{"prior month (now-35d)", now.Add(-35 * 24 * time.Hour)},
		{"current month (now)", now},
		{"future month (now+40d)", now.Add(40 * 24 * time.Hour)},
	}

	var insertedIDs []uuid.UUID

	t.Run("insert audit rows spanning three partitions", func(t *testing.T) {
		for _, ts := range timestamps {
			entry := &domain.TransactionAudit{
				ID:            uuid.New(),
				TransactionID: tx.ID,
				FromStatus:    domain.StatusPending,
				ToStatus:      domain.StatusHeld,
				ActorUserID:   payer,
				OccurredAt:    ts.occurredAt,
			}

			require.NoError(t, auditStore.Append(ctx, entry), "Append failed for %s", ts.name)
			insertedIDs = append(insertedIDs, entry.ID)
		}
	})

	t.Run("all inserted rows visible via parent table query", func(t *testing.T) {
		// Query the parent table directly; PG routes the scan to child partitions.
		const q = `SELECT COUNT(*) FROM transaction_audit WHERE transaction_id = $1`

		var count int
		row := pool.QueryRow(ctx, q, tx.ID)
		require.NoError(t, row.Scan(&count))
		// We inserted 3 rows above (plus potentially 0 from prior subtests since tx is fresh).
		assert.GreaterOrEqual(t, count, 3, "expected at least 3 rows across all partitions")
	})

	t.Run("each row queryable by its exact id + occurred_at (partition pruning)", func(t *testing.T) {
		// Querying by (id, occurred_at) exercises partition pruning: PG uses occurred_at
		// to select only the relevant child partition — confirms the composite PK works.
		const q = `SELECT id FROM transaction_audit WHERE id = $1 AND occurred_at = $2`

		for i, ts := range timestamps {
			id := insertedIDs[i]
			var got uuid.UUID
			row := pool.QueryRow(ctx, q, id, ts.occurredAt)
			require.NoError(t, row.Scan(&got), "row not found for %s", ts.name)
			assert.Equal(t, id, got, "id mismatch for %s", ts.name)
		}
	})

	t.Run("child partition names exist for current and adjacent months", func(t *testing.T) {
		// Verify the DO $$ bootstrap created the expected named partitions.
		// Check the prior month, current month, and next month.
		for _, offset := range []int{-1, 0, 1} {
			// Compute exact month start by adding months to the year/month boundary.
			base := truncMonth(now)
			expected := "transaction_audit_p" + addMonths(base, offset).Format("200601")

			const q = `SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'r')`
			var exists bool
			row := pool.QueryRow(ctx, q, expected)
			require.NoError(t, row.Scan(&exists))
			assert.True(t, exists, "expected partition %s to exist", expected)
		}
	})
}

// TestAuditStore_ForUpdateWritesPartitionedAudit verifies that the SELECT FOR UPDATE
// transition path (used by service.transition()) successfully writes its audit row into
// the partitioned transaction_audit table as part of the same DB transaction.
func TestAuditStore_ForUpdateWritesPartitionedAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn)
	require.NoError(t, err)

	defer pool.Close()

	txStore := postgres.NewTransactionStore(pool)
	txMgr := postgres.NewTxManager(pool)

	payer := uuid.New()
	payee := uuid.New()
	now := time.Now().UTC()

	amount, _ := decimal.NewFromString("750.00") // test fixture literal — always valid
	original := &domain.Transaction{
		ID:             uuid.New(),
		PayerUserID:    payer,
		PayeeUserID:    payee,
		Amount:         amount,
		Currency:       "TWD",
		Status:         domain.StatusHeld,
		IdempotencyKey: uuid.New().String(),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	require.NoError(t, txStore.Create(ctx, original))

	err = txMgr.WithTx(ctx, func(ctx context.Context, txs storeTransactionStore, audits storeAuditStore) error {
		_, lockErr := txs.GetByIDForUpdate(ctx, original.ID)
		require.NoError(t, lockErr)

		updateErr := txs.UpdateStatus(ctx, original.ID, domain.StatusReleased)
		require.NoError(t, updateErr)

		// Write audit row into the partitioned table inside the same DB transaction.
		audit := &domain.TransactionAudit{
			ID:            uuid.New(),
			TransactionID: original.ID,
			FromStatus:    domain.StatusHeld,
			ToStatus:      domain.StatusReleased,
			ActorUserID:   payer,
			OccurredAt:    now,
		}

		return audits.Append(ctx, audit)
	})
	require.NoError(t, err, "FOR UPDATE + partitioned audit append must succeed atomically")

	// Verify the status change committed.
	updated, getErr := txStore.GetByID(ctx, original.ID)
	require.NoError(t, getErr)
	assert.Equal(t, domain.StatusReleased, updated.Status)

	// Verify the audit row landed in the partitioned table.
	const q = `SELECT COUNT(*) FROM transaction_audit WHERE transaction_id = $1`

	var count int
	require.NoError(t, pool.QueryRow(ctx, q, original.ID).Scan(&count))
	assert.Equal(t, 1, count, "exactly one audit row expected in partitioned table")
}

// TestNewPool_ReservedWordSchema verifies that NewPool correctly creates and uses a schema
// whose name is a PostgreSQL reserved word (e.g. "user"). Without double-quoting the
// identifier in CREATE SCHEMA and SET search_path, PG returns syntax error 42601.
//
// This test covers the fix introduced in pool.go: pgx.Identifier{schema}.Sanitize()
// produces a properly double-quoted identifier, making reserved words work at runtime.
func TestNewPool_ReservedWordSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)

	// "user" is a PostgreSQL reserved word. Before the fix, passing it unquoted produced:
	//   ERROR: syntax error at or near "user" (SQLSTATE 42601)
	const reservedWordSchema = "user"

	// NewPool with schema="user": must CREATE SCHEMA "user" and set search_path = "user".
	pool, err := postgres.NewPoolWithConfig(ctx, dsn, postgres.PoolConfig{
		Schema:   reservedWordSchema,
		MaxConns: 4,
		MinConns: 1,
	})
	require.NoError(t, err, "NewPoolWithConfig must succeed for reserved-word schema %q", reservedWordSchema)

	t.Cleanup(pool.Close)

	t.Run("schema user exists in pg_namespace", func(t *testing.T) {
		const q = `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, q, reservedWordSchema).Scan(&exists))
		assert.True(t, exists, "schema %q must exist in pg_namespace after NewPool", reservedWordSchema)
	})

	t.Run("search_path is set to user schema on acquired connections", func(t *testing.T) {
		// current_schema() returns the first schema in search_path that exists.
		var currentSchema string
		require.NoError(t, pool.QueryRow(ctx, "SELECT current_schema()").Scan(&currentSchema))
		assert.Equal(t, reservedWordSchema, currentSchema, "search_path must point to the %q schema", reservedWordSchema)
	})

	t.Run("can create and query a table inside the user schema", func(t *testing.T) {
		// Create the service's main table inside the "user" schema by relying on search_path.
		// This exercises the full runtime path: schema created, search_path set, table usable.
		_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS pool_schema_probe (id SERIAL PRIMARY KEY, label TEXT)`)
		require.NoError(t, err, "CREATE TABLE in reserved-word schema must succeed")

		_, err = pool.Exec(ctx, `INSERT INTO pool_schema_probe (label) VALUES ('reserved-word-schema-test')`)
		require.NoError(t, err, "INSERT into reserved-word schema table must succeed")

		const checkQ = `
			SELECT EXISTS (
				SELECT 1
				FROM   information_schema.tables
				WHERE  table_schema = $1
				AND    table_name   = 'pool_schema_probe'
			)`

		var exists bool
		require.NoError(t, pool.QueryRow(ctx, checkQ, reservedWordSchema).Scan(&exists))
		assert.True(t, exists, "pool_schema_probe must exist in schema %q", reservedWordSchema)
	})
}

// truncMonth returns the first instant of the month containing t (UTC).
func truncMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// addMonths returns the first instant of the month offset months from base (UTC).
// Uses time.Date to handle month-boundary arithmetic correctly (e.g. Jan → Dec).
func addMonths(base time.Time, offset int) time.Time {
	y, m, _ := base.Date()
	return time.Date(y, time.Month(int(m)+offset), 1, 0, 0, 0, 0, time.UTC)
}
