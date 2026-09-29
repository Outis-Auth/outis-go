// Command worker is a service that runs authorized Outis intents with its
// own clients. It polls Outis, serves a health check, and on SIGTERM stops
// claiming new work, finishes what's running, then exits.
//
//	OUTIS_API_KEY=outis_sk_... OUTIS_INTENT_KEY=... go run ./examples/worker
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	outis "github.com/outis-auth/outis-go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("worker", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	keys, err := outis.ParseIntentKeys(firstSet(os.Getenv("OUTIS_INTENT_KEYS"), os.Getenv("OUTIS_INTENT_KEY")))
	if err != nil {
		return err
	}
	client := outis.New(os.Getenv("OUTIS_API_KEY"))
	w := outis.NewWorker(client, outis.WorkerOptions{
		Keys:        keys,
		Allow:       []string{"ledger.*", "db.restore"},
		Concurrency: 4,
		OnResult: func(r outis.Result) {
			slog.Info("outis run", "request", r.RequestID, "call", r.Client+"."+r.Method, "status", r.Status, "ref", r.Reference, "err", r.Err)
		},
	})

	// Register your real clients here, built with your own credentials.
	w.Register("ledger", &Ledger{})
	w.Handle("db.restore", func(ctx context.Context, x outis.Execution, args json.RawMessage) (string, error) {
		var in []json.RawMessage
		var db string
		var opts struct {
			Snapshot string `json:"snapshot"`
		}
		if err := json.Unmarshal(args, &in); err != nil || len(in) != 2 {
			return "", fmt.Errorf("want [db, {snapshot}], got %s", args)
		}
		if err := errors.Join(json.Unmarshal(in[0], &db), json.Unmarshal(in[1], &opts)); err != nil {
			return "", err
		}
		return restore(ctx, db, opts.Snapshot, x.IdempotencyKey)
	})

	var draining atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			http.Error(rw, "draining", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(rw, "ok")
	})
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health server", "err", err)
		}
	}()

	ran := make(chan error, 1)
	go func() { ran <- w.Run(ctx, 15*time.Second) }()

	<-ctx.Done()
	draining.Store(true)
	slog.Info("draining")
	err = <-ran

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(err, srv.Shutdown(sctx))
}

// Ledger stands in for a client of your own.
type Ledger struct{}

// Entry is a ledger posting as the proposer sent it.
type Entry struct {
	Account string `json:"account"`
	Cents   int64  `json:"cents"`
}

// Receipt is what a posting returns; its ID becomes the run's reference.
type Receipt struct{ ID string }

// Post is reached by an intent with client "ledger" and method "post".
func (l *Ledger) Post(ctx context.Context, e Entry) (*Receipt, error) {
	return &Receipt{ID: "posting_" + outis.IdempotencyKey(ctx)}, nil
}

func restore(ctx context.Context, db, snapshot, idempotencyKey string) (string, error) {
	return "restore:" + db + "@" + snapshot, ctx.Err()
}

func firstSet(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
