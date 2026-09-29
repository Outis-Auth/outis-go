// Command guard asks Outis before a payout runs, three ways: waiting inline,
// waiting on a goroutine, and handing the call to a worker.
//
//	OUTIS_API_KEY=outis_sk_... OUTIS_INTENT_KEY=... go run ./examples/guard
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	outis "github.com/outis-auth/outis-go"
	stripeguard "github.com/outis-auth/outis-go/recipes/stripe"
)

func main() {
	key, err := outis.ParseIntentKey(os.Getenv("OUTIS_INTENT_KEY"))
	if err != nil {
		log.Fatal(err)
	}
	client := outis.New(os.Getenv("OUTIS_API_KEY"), outis.WithRequester("payouts-api"), outis.WithIntentKey(key))
	ctx := context.Background()
	payouts := &Payouts{}

	// Wait here, then run the transfer only if it was authorized.
	params := &TransferParams{Amount: ptr(int64(2500000)), Currency: ptr("usd"), Destination: ptr("acct_9f2")}
	shown, err := stripeguard.Transfer{Amount: params.Amount, Currency: params.Currency, Destination: params.Destination}.ShowApprovers()
	if err != nil {
		log.Fatal(err)
	}
	tr, err := outis.GuardFunc(ctx, client, outis.GuardOptions{
		Action:        stripeguard.ActionTransfersCreate,
		ShowApprovers: shown,
		Wait:          5 * time.Minute,
	}, func(ctx context.Context) (*Transfer, error) {
		return payouts.CreateTransfer(ctx, params)
	})
	report("inline", tr, err)

	// Keep going while the approvers decide.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := client.Guard(ctx, outis.GuardOptions{
			Action:        "db.restore",
			ShowApprovers: map[string]string{"db": "payments", "snapshot": "2026-09-27T04:00Z"},
			Wait:          10 * time.Minute,
		})
		if err != nil {
			log.Printf("restore not approved: %v", err)
			return
		}
		log.Print("restore approved, running it")
	}()

	// Approval could take days, so a worker runs it later.
	d, err := client.Defer(ctx, outis.GuardOptions{
		Action:         stripeguard.ActionTransfersCreate,
		ShowApprovers:  shown,
		IdempotencyKey: "payout-7731",
		DeferTo:        &outis.DeferTo{Worker: "stripe", Call: "v1transfers.create", Args: []any{params}},
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("deferred as %s; a worker runs it once authorized", d.ID)
	<-done
}

func report(how string, tr *Transfer, err error) {
	var notAuth *outis.NotAuthorizedError
	var timedOut *outis.WaitTimeoutError
	switch {
	case errors.As(err, &notAuth):
		log.Printf("%s: %s", how, notAuth.Outcome)
	case errors.As(err, &timedOut):
		log.Printf("%s: still pending, check %s later", how, timedOut.RequestID)
	case err != nil:
		log.Printf("%s: %v", how, err)
	default:
		log.Printf("%s: sent %s", how, tr.ID)
	}
}

// TransferParams mirrors stripe-go's TransferCreateParams.
type TransferParams struct {
	Amount      *int64  `json:"amount"`
	Currency    *string `json:"currency"`
	Destination *string `json:"destination"`
}

// Transfer is what a transfer returns.
type Transfer struct{ ID string }

// Payouts stands in for a Stripe client of your own.
type Payouts struct{}

// CreateTransfer stands in for stripe-go's V1Transfers.Create.
func (p *Payouts) CreateTransfer(ctx context.Context, params *TransferParams) (*Transfer, error) {
	return &Transfer{ID: "tr_" + *params.Destination}, ctx.Err()
}

func ptr[T any](v T) *T { return &v }
