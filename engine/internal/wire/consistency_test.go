package wire

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestConsistencyBankTransfers is a Jepsen-lite invariant harness: many workers
// concurrently move money between accounts inside transactions, while a checker
// reads the global total. Under first-committer-wins snapshot isolation the sum
// of balances must remain invariant — no lost updates, no torn transactions.
//
// Each transfer is BEGIN; read both balances; UPDATE both; COMMIT, retrying the
// whole transaction on a 40001 serialization conflict (the standard SI client
// contract). Without write-conflict detection this test exposes lost updates;
// with it, the invariant holds.
func TestConsistencyBankTransfers(t *testing.T) {
	const (
		accounts   = 8
		startEach  = 1000
		workers    = 6
		perWorker  = 60
	)
	total := accounts * startEach

	addr := startTestServer(t)
	ctx := context.Background()

	// Seed accounts.
	seed := pgxConnect(t, addr, false)
	if _, err := seed.Exec(ctx, "CREATE TABLE acct (id INT PRIMARY KEY, bal INT NOT NULL)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < accounts; i++ {
		if _, err := seed.Exec(ctx, "INSERT INTO acct (id, bal) VALUES ($1, $2)", i, startEach); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	seed.Close(ctx)

	var conflicts atomic.Int64
	var transfers atomic.Int64

	// One connection per worker (pgx conns are not concurrency-safe).
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			conn := pgxConnect(t, addr, false)
			defer conn.Close(ctx)
			for i := 0; i < perWorker; i++ {
				from := rng.Intn(accounts)
				to := rng.Intn(accounts)
				if from == to {
					continue
				}
				amt := 1 + rng.Intn(50)
				if err := transfer(ctx, conn, from, to, amt, &conflicts); err != nil {
					t.Errorf("transfer: %v", err)
					return
				}
				transfers.Add(1)
			}
		}(int64(w) + 1)
	}

	// Checker: the visible total must always equal the seeded total.
	var checkErr atomic.Pointer[string]
	go func() {
		c := pgxConnect(t, addr, false)
		defer c.Close(ctx)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var sum int
			if err := c.QueryRow(ctx, "SELECT sum(bal) FROM acct").Scan(&sum); err != nil {
				continue
			}
			if sum != total {
				s := fmt.Sprintf("invariant violated mid-run: sum=%d want=%d", sum, total)
				checkErr.Store(&s)
				return
			}
		}
	}()

	wg.Wait()
	close(stop)

	if msg := checkErr.Load(); msg != nil {
		t.Fatal(*msg)
	}

	// Final invariant: total preserved exactly.
	conn := pgxConnect(t, addr, false)
	defer conn.Close(ctx)
	var sum int
	if err := conn.QueryRow(ctx, "SELECT sum(bal) FROM acct").Scan(&sum); err != nil {
		t.Fatalf("final sum: %v", err)
	}
	if sum != total {
		t.Fatalf("final invariant: sum=%d want=%d", sum, total)
	}
	t.Logf("bank harness: %d transfers committed, %d conflicts retried, total preserved at %d",
		transfers.Load(), conflicts.Load(), total)
}

// transfer moves amt from->to in a transaction, retrying on serialization
// conflict (SQLSTATE 40001). Skips when the source has insufficient funds.
func transfer(ctx context.Context, conn *pgx.Conn, from, to, amt int, conflicts *atomic.Int64) error {
	for {
		err := func() error {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)

			var fromBal int
			if err := tx.QueryRow(ctx, "SELECT bal FROM acct WHERE id = $1", from).Scan(&fromBal); err != nil {
				return err
			}
			if fromBal < amt {
				return tx.Rollback(ctx) // nothing to move; end cleanly
			}
			var toBal int
			if err := tx.QueryRow(ctx, "SELECT bal FROM acct WHERE id = $1", to).Scan(&toBal); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE acct SET bal = $1 WHERE id = $2", fromBal-amt, from); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE acct SET bal = $1 WHERE id = $2", toBal+amt, to); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		if err == nil {
			return nil
		}
		if isSerializationConflict(err) {
			conflicts.Add(1)
			continue // retry the whole transaction
		}
		return err
	}
}

func isSerializationConflict(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "40001"
	}
	return false
}
