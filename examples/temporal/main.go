//go:build temporal

// Command temporal runs a Temporal worker whose workflow waits on Outis, plus
// the bridge that signals it. Build it with -tags temporal in a module that
// requires go.temporal.io/sdk.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	outis "github.com/outis-auth/outis-go"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	key, err := outis.ParseIntentKey(os.Getenv("OUTIS_INTENT_KEY"))
	if err != nil {
		log.Fatal(err)
	}
	oc := outis.New(os.Getenv("OUTIS_API_KEY"), outis.WithIntentKey(key))
	ow := outis.NewWorker(oc, outis.WorkerOptions{Keys: [][]byte{key}})
	ow.Register("payouts", &Payouts{})

	tc, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer tc.Close()

	go func() {
		srv := &http.Server{Addr: ":8080", Handler: Bridge(tc, os.Getenv("OUTIS_WEBHOOK_SECRET")), ReadHeaderTimeout: 5 * time.Second}
		log.Fatal(srv.ListenAndServe())
	}()

	w := worker.New(tc, "payouts", worker.Options{})
	w.RegisterWorkflow(PayoutWorkflow)
	w.RegisterActivity(&Activities{Outis: oc, Worker: ow})
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatal(err)
	}
}

// Payouts stands in for your payouts client.
type Payouts struct{}

// Release is reached by the intent's "payouts.release".
func (p *Payouts) Release(ctx context.Context, account string, cents int64) (string, error) {
	return "po_" + outis.IdempotencyKey(ctx), nil
}
