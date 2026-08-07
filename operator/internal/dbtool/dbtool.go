// Package dbtool is a simple query-loop runner used by Workload actions to
// generate realistic database load. It is compiled into the operator binary
// and invoked as `rca-lab dbtool ...` inside scenario Jobs.
package dbtool

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

type queryList []string

func (q *queryList) String() string     { return strings.Join(*q, "; ") }
func (q *queryList) Set(v string) error { *q = append(*q, v); return nil }

// Run executes the dbtool subcommand with the given arguments.
func Run(args []string) error {
	fs := flag.NewFlagSet("dbtool", flag.ContinueOnError)
	engine := fs.String("engine", "", "database engine: postgres|mysql")
	interval := fs.Duration("interval", time.Second, "pause between iterations per worker")
	concurrency := fs.Int("concurrency", 1, "number of parallel workers")
	deadline := fs.Duration("deadline", 0, "optional self-stop after this duration")
	var queries queryList
	fs.Var(&queries, "query", "SQL statement to run in a loop (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	var driver string
	switch *engine {
	case "postgres":
		driver = "postgres"
	case "mysql":
		driver = "mysql"
	default:
		return fmt.Errorf("--engine must be postgres or mysql, got %q", *engine)
	}
	if len(queries) == 0 {
		return fmt.Errorf("at least one --query is required")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL environment variable is required")
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(*concurrency + 1)
	db.SetMaxIdleConns(*concurrency + 1)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if *deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *deadline)
		defer cancel()
	}

	log.Info("dbtool starting", "engine", *engine, "queries", len(queries),
		"concurrency", *concurrency, "interval", interval.String())

	var wg sync.WaitGroup
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			runWorker(ctx, log, db, worker, queries, *interval)
		}(w)
	}
	wg.Wait()
	log.Info("dbtool exiting")
	return nil
}

func runWorker(ctx context.Context, log *slog.Logger, db *sql.DB, worker int, queries []string, interval time.Duration) {
	for {
		attrs := []any{"worker", worker}
		batchStart := time.Now()
		for i, q := range queries {
			if ctx.Err() != nil {
				return
			}
			start := time.Now()
			rows, err := runQuery(ctx, db, q)
			elapsed := time.Since(start)
			attrs = append(attrs, fmt.Sprintf("q%d_ms", i+1), elapsed.Milliseconds())
			if err != nil {
				// Errors are expected while the database is overloaded: log
				// and keep going.
				attrs = append(attrs, fmt.Sprintf("q%d_error", i+1), err.Error())
				continue
			}
			attrs = append(attrs, fmt.Sprintf("q%d_rows", i+1), rows)
		}
		attrs = append(attrs, "total_ms", time.Since(batchStart).Milliseconds())
		log.Info("iteration", attrs...)

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func runQuery(ctx context.Context, db *sql.DB, q string) (int, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}
