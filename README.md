![Outis Go SDK and CLI](assets/header.png)

The Go SDK and CLI for [Outis](https://outis.tech). Use it to get sign-off from real people before your code does something risky.

## How Outis works

1. Your code asks Outis to approve an operation, like a large payout.
2. Outis shows the details to the right people on a physical device.
3. They approve or reject it by turning a key.
4. Once it's approved, your code runs the operation with its own credentials.

Outis never runs the operation, and it never sees your secrets, API keys or other credentials. It decides whether the right people approved, and keeps a record of who did.

## Install

```sh
go get github.com/outis-auth/outis-go
```

You need Go 1.25 or newer. The SDK uses only the standard library.

## Quickstart

```go
client := outis.New(os.Getenv("OUTIS_API_KEY"), outis.WithRequester("payouts-api"))

_, err := client.Guard(ctx, outis.GuardOptions{
	Action:        "db.restore",
	ShowApprovers: map[string]string{"db": "payments", "snapshot": "2026-09-27T04:00Z"},
	Wait:          5 * time.Minute,
})
if err != nil {
	return err // rejected, timed out, or Outis couldn't be reached
}
return restore(ctx)
```

`Guard` blocks until people decide or `Wait` runs out. A nil error means the request was approved, so go ahead and run the operation. Approvers see exactly what's in `ShowApprovers`, and their approval covers those values.

To keep going while you wait, use a goroutine:

```go
go func() {
	if _, err := client.Guard(ctx, opts); err == nil {
		restore(ctx)
	}
}()
```

Cancel `ctx` to stop waiting. The request stays open in Outis. Inside an HTTP handler, give the goroutine a context that outlives the request.

## Pick a path

| Your situation | Use |
|---|---|
| Approval takes minutes | `client.Guard` with `Wait` |
| Approval could take hours or days | `client.Defer` with `DeferTo`, plus a worker |
| Guarding one call you already make | `outis.GuardFunc` |
| Calling Stripe | `recipes/stripe` |

## Defer to a worker

`Wait` is capped at 30 minutes, and a waiting goroutine dies with its process. When approval might take longer, hand the call to a worker instead:

```go
key, _ := outis.ParseIntentKey(os.Getenv("OUTIS_INTENT_KEY"))
client := outis.New(os.Getenv("OUTIS_API_KEY"), outis.WithIntentKey(key))

d, err := client.Defer(ctx, outis.GuardOptions{
	Action:         "stripe.transfers.create",
	Requester:      "payouts-api",
	ShowApprovers:  map[string]string{"amount": "2500000", "currency": "usd", "destination": "acct_9f2"},
	IdempotencyKey: "payout-7731",
	DeferTo:        &outis.DeferTo{Worker: "stripe", Call: "v1transfers.create", Args: []any{params}},
})
```

`Defer` returns as soon as the request exists, before anyone approves and before anything runs. The call is encrypted with your intent key, so Outis can't read it or change it. Generate a key once with `outis.GenerateIntentKey()`. Keep it in your secret store as `OUTIS_INTENT_KEY`.

## The worker

A worker runs deferred calls once they're approved. It uses your own clients and credentials.

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

w := outis.NewWorker(outis.New(os.Getenv("OUTIS_API_KEY")), outis.WorkerOptions{Keys: keys})
w.Register("stripe", stripe.NewClient(os.Getenv("STRIPE_KEY")), outis.InjectIdempotencyKey())
return w.Run(ctx, 15*time.Second)
```

Before it runs anything, the worker checks that the request was approved for this exact call. `Run` polls Outis on the interval you give it. When `ctx` ends, it stops taking new work and finishes what's in flight. Run `outis init worker` for a full starter service.

When no client you can register makes the call, write a handler with `Handle`. It gets the `Execution` (the request, the claim and the idempotency key) and the call's arguments as the raw JSON array the caller sealed, so you decode them into whatever types you need. The string you return is reported as the reference.

```go
w.Handle("db.restore", func(ctx context.Context, x outis.Execution, args json.RawMessage) (string, error) {
	var in []string // ["payments", "snap_0927"]
	if err := json.Unmarshal(args, &in); err != nil || len(in) != 2 {
		return "", fmt.Errorf("want [db, snapshot], got %s", args)
	}
	return restore(ctx, in[0], in[1], x.IdempotencyKey)
})
```

On a cron job or a serverless function, where nothing stays running between invocations, call `Poll` instead of `Run`. It lists what's approved once, runs it `Concurrency` at a time, waits for all of it, and returns how many ran:

```go
ran, err := w.Poll(ctx)
```

## GuardFunc

`GuardFunc` waits for approval, then runs your function. If the request isn't approved, the function never runs.

```go
transfer, err := outis.GuardFunc(ctx, client, outis.GuardOptions{
	Action:        "stripe.transfers.create",
	ShowApprovers: map[string]string{"amount": "2500000", "currency": "usd", "destination": "acct_9f2"},
	Wait:          5 * time.Minute,
}, func(ctx context.Context) (*stripe.Transfer, error) {
	return sc.V1Transfers.Create(ctx, params)
})
```

## Stripe recipes

`recipes/stripe` picks the fields from stripe-go params that approvers should see for transfers, payouts, refunds and customer deletes. That means IDs, amounts in minor units, currencies, a few settings like the refund reason, and a shortened description. Metadata and emails stay out, and the package doesn't import stripe-go.

```go
import stripeguard "github.com/outis-auth/outis-go/recipes/stripe"

shown, err := stripeguard.Transfer{Amount: params.Amount, Currency: params.Currency, Destination: params.Destination}.ShowApprovers()
if err != nil {
	return err // a required field is missing
}
opts := outis.GuardOptions{
	Action:        stripeguard.ActionTransfersCreate,
	ShowApprovers: shown,
	Wait:          5 * time.Minute,
}
```

## Errors

Check errors with `errors.As`:

| Type | Meaning |
|---|---|
| `*outis.NotAuthorizedError` | People said no. `Outcome` is `denied`, `expired` or `aborted`. |
| `*outis.WaitTimeoutError` | `Wait` ran out. `RequestID` lets you check again later. |
| `*outis.APIError` | The API refused. See `StatusCode` and `Code`. |

## The CLI

```sh
go install github.com/outis-auth/outis-go/cmd/outis@latest
outis gate -action db.restore -requester keith -param db=payments -timeout 5m ./restore.sh
```

`outis gate` waits for approval, then runs the command. It also has `request`, `get`, `wait`, `hash`, `verify-webhook` and `init worker`. Prebuilt binaries for Linux, macOS and Windows are on the [releases page](https://github.com/outis-auth/outis-go/releases).

**`outis gate` isn't a security boundary on its own.** If someone can run `./restore.sh` directly, gate stops nothing. It only protects an operation when the credentials that operation needs exist only where gate runs, like a locked-down CI step or a dedicated job.

## Learn more

The [developer center](https://developers.outis.tech) has the details:

- [The full API](https://developers.outis.tech/api/)
- [Webhooks](https://developers.outis.tech/guides/webhooks/)
- [Durable execution](https://developers.outis.tech/guides/durable-execution/)
- [Engines like Temporal](https://developers.outis.tech/guides/engines/)
- [The CLI](https://developers.outis.tech/guides/cli/)
- [The low-level API](https://developers.outis.tech/guides/sdks/): `client.Requests`, `client.Intents`, webhook checks and manual worker control
