package tunnel

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// mockTx implements pgx.Tx for testing AllocatePort. AllocatePort issues three
// kinds of statements: the advisory-lock Exec, a nextval() QueryRow (the
// monotonic round-robin cursor), and an EXISTS QueryRow per candidate port
// (whether an active instance holds it). The mock dispatches on the SQL text.
type mockTx struct {
	cursor      int64        // last value handed out by nextval()
	activePorts map[int]bool // ports an active instance currently holds
	execErr     error        // error from Exec (advisory lock)
	nextvalErr  error        // error from the nextval QueryRow
	existsErr   error        // error from the EXISTS QueryRow
	execCalls   int
}

func (m *mockTx) Begin(ctx context.Context) (pgx.Tx, error) { return m, nil }
func (m *mockTx) Commit(ctx context.Context) error          { return nil }
func (m *mockTx) Rollback(ctx context.Context) error        { return nil }
func (m *mockTx) CopyFrom(ctx context.Context, t pgx.Identifier, c []string, s pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (m *mockTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults { return nil }
func (m *mockTx) LargeObjects() pgx.LargeObjects                               { return pgx.LargeObjects{} }
func (m *mockTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (m *mockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.execCalls++
	if m.execErr != nil {
		return pgconn.CommandTag{}, m.execErr
	}
	return pgconn.CommandTag{}, nil
}
func (m *mockTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}
func (m *mockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "nextval"):
		if m.nextvalErr != nil {
			return &mockRow{err: m.nextvalErr}
		}
		m.cursor++
		return &mockRow{int64Val: m.cursor}
	case strings.Contains(sql, "EXISTS"):
		if m.existsErr != nil {
			return &mockRow{err: m.existsErr}
		}
		port, _ := args[0].(int)
		return &mockRow{boolVal: m.activePorts[port]}
	default:
		return &mockRow{}
	}
}
func (m *mockTx) Conn() *pgx.Conn { return nil }

// mockRow implements pgx.Row, scanning into *int64 or *bool by dest type.
type mockRow struct {
	int64Val int64
	boolVal  bool
	err      error
}

func (r *mockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) == 0 {
		return nil
	}
	switch d := dest[0].(type) {
	case *int64:
		*d = r.int64Val
	case *bool:
		*d = r.boolVal
	}
	return nil
}

func TestAllocatePort_FirstIsMinPort(t *testing.T) {
	// Fresh sequence: nextval=1 -> MinPort. Nothing active.
	tx := &mockTx{activePorts: map[int]bool{}}
	port, err := AllocatePort(context.Background(), tx, DefaultPortRange())
	if err != nil {
		t.Fatalf("AllocatePort() error: %v", err)
	}
	if port != MinPort {
		t.Errorf("AllocatePort() = %d, want %d", port, MinPort)
	}
	if tx.execCalls != 1 {
		t.Errorf("advisory lock Exec calls = %d, want 1", tx.execCalls)
	}
}

func TestAllocatePort_Monotonic(t *testing.T) {
	// The cursor advances per allocation (10000, 10001, 10002, ...) regardless
	// of what was freed -- the whole point: a just-freed port is not reused
	// until the range cycles, so it can't collide with a lingering frps
	// registration on the shared gateway.
	tx := &mockTx{activePorts: map[int]bool{}}
	for i, want := range []int{MinPort, MinPort + 1, MinPort + 2} {
		got, err := AllocatePort(context.Background(), tx, DefaultPortRange())
		if err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
		if got != want {
			t.Errorf("alloc %d = %d, want %d", i, got, want)
		}
	}
}

func TestAllocatePort_SkipsActive(t *testing.T) {
	// The cursor lands on 10000 and 10001 (both held by active instances) and
	// must advance to the first free port, 10002.
	tx := &mockTx{activePorts: map[int]bool{MinPort: true, MinPort + 1: true}}
	port, err := AllocatePort(context.Background(), tx, DefaultPortRange())
	if err != nil {
		t.Fatalf("AllocatePort() error: %v", err)
	}
	if port != MinPort+2 {
		t.Errorf("AllocatePort() = %d, want %d", port, MinPort+2)
	}
}

func TestAllocatePort_WrapsAroundRange(t *testing.T) {
	rangeSize := MaxPort - MinPort + 1
	// Seed the cursor so the next value maps to MaxPort, then the one after
	// wraps back to MinPort -- proving the modulo cycle.
	tx := &mockTx{cursor: int64(rangeSize - 1), activePorts: map[int]bool{}}
	port, err := AllocatePort(context.Background(), tx, DefaultPortRange())
	if err != nil {
		t.Fatalf("AllocatePort() error: %v", err)
	}
	if port != MaxPort {
		t.Errorf("AllocatePort() = %d, want %d (MaxPort)", port, MaxPort)
	}
	port2, err := AllocatePort(context.Background(), tx, DefaultPortRange())
	if err != nil {
		t.Fatalf("AllocatePort() second error: %v", err)
	}
	if port2 != MinPort {
		t.Errorf("AllocatePort() wrapped = %d, want %d (MinPort)", port2, MinPort)
	}
}

func TestAllocatePort_Exhaustion(t *testing.T) {
	// Every port in the range is held by an active instance -> exhausted, and
	// the loop terminates (bounded by rangeSize) rather than spinning forever.
	all := map[int]bool{}
	for p := MinPort; p <= MaxPort; p++ {
		all[p] = true
	}
	tx := &mockTx{activePorts: all}
	_, err := AllocatePort(context.Background(), tx, DefaultPortRange())
	if err == nil {
		t.Fatal("AllocatePort() should error when range exhausted, got nil")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("error should mention exhaustion, got: %v", err)
	}
}

func TestAllocatePort_AdvisoryLockFailure(t *testing.T) {
	tx := &mockTx{activePorts: map[int]bool{}, execErr: fmt.Errorf("connection lost")}
	if _, err := AllocatePort(context.Background(), tx, DefaultPortRange()); err == nil {
		t.Error("AllocatePort() should error when advisory lock fails, got nil")
	}
}

func TestAllocatePort_CursorQueryFailure(t *testing.T) {
	tx := &mockTx{activePorts: map[int]bool{}, nextvalErr: fmt.Errorf("query timeout")}
	if _, err := AllocatePort(context.Background(), tx, DefaultPortRange()); err == nil {
		t.Error("AllocatePort() should error when cursor query fails, got nil")
	}
}

func TestAllocatePort_AvailabilityQueryFailure(t *testing.T) {
	tx := &mockTx{activePorts: map[int]bool{}, existsErr: fmt.Errorf("query timeout")}
	if _, err := AllocatePort(context.Background(), tx, DefaultPortRange()); err == nil {
		t.Error("AllocatePort() should error when availability query fails, got nil")
	}
}
