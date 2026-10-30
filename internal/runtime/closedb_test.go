package runtime

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A connection asked for before the pool closes (its one connection thrown
// away while a query waits) is opened after database/sql's Close returns;
// closeDB returns only once it is closed again, and SQLite's files with it:
// nothing touches a closed runtime's folder (a TempDir's cleanup failed on
// it, "directory not empty").
func TestCloseDBWaitsForTheOpener(t *testing.T) {
	for i := range 20 {
		dir := t.TempDir()
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "k.db")+"?_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1) // as kairo's store has it
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, err := db.ExecContext(ctx, `CREATE TABLE t (x)`); err != nil {
			t.Fatal(err)
		}
		c, err := db.Conn(ctx) // the one connection
		if err != nil {
			t.Fatal(err)
		}
		waiter, stopWaiting := context.WithCancel(ctx)
		waited := make(chan struct{})
		go func() {
			defer close(waited)
			var x int
			_ = db.QueryRowContext(waiter, `SELECT 1`).Scan(&x)
		}()
		for db.Stats().WaitCount == 0 { // the query waits for it
			if ctx.Err() != nil {
				t.Fatal("the query did not wait for the connection")
			}
			time.Sleep(time.Millisecond)
		}
		// Thrown away: the opener is asked for one for the query.
		_ = c.Raw(func(any) error { return driver.ErrBadConn })
		_ = c.Close()
		if err := closeDB(db); err != nil {
			t.Fatal(err)
		}
		if n := db.Stats().OpenConnections; n != 0 {
			t.Fatalf("run %d: %d connections after closing", i, n)
		}
		es, _ := os.ReadDir(dir)
		if len(es) != 1 {
			var names []string
			for _, e := range es {
				names = append(names, e.Name())
			}
			t.Fatalf("run %d: the folder after closing: %v", i, names)
		}
		stopWaiting()
		<-waited
		cancel()
	}
}
