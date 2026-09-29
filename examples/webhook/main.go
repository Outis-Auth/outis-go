// Command webhook runs authorized Outis intents as their webhooks arrive,
// with no polling. It accepts deliveries from an org webhook endpoint and
// from a request's callback_url on the same route.
//
//	OUTIS_API_KEY=outis_sk_... OUTIS_INTENT_KEY=... OUTIS_WEBHOOK_SECRET=whsec_... go run ./examples/webhook
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	outis "github.com/outis-auth/outis-go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("webhook", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	key, err := outis.ParseIntentKey(os.Getenv("OUTIS_INTENT_KEY"))
	if err != nil {
		return err
	}
	apiKey := os.Getenv("OUTIS_API_KEY")
	w := outis.NewWorker(outis.New(apiKey), outis.WorkerOptions{Keys: [][]byte{key}})
	w.Register("mailer", &Mailer{})

	mux := http.NewServeMux()
	mux.Handle("POST /outis/webhook", w.HandlerKeys(
		[]byte(os.Getenv("OUTIS_WEBHOOK_SECRET")),
		outis.CallbackSecret(apiKey),
	))
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(sctx)
	w.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Mailer stands in for a client of your own.
type Mailer struct{}

// Sent is a send's result; its ID becomes the run's reference.
type Sent struct{ ID string }

// Broadcast is reached by an intent with client "mailer" and method "broadcast".
func (m *Mailer) Broadcast(ctx context.Context, list, template string) (Sent, error) {
	return Sent{ID: "bc_" + outis.IdempotencyKey(ctx)}, nil
}
