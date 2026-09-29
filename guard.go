package outis

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// GuardOptions describes one guarded operation. Set exactly one of Wait
// (for [Client.Guard] and [GuardFunc]) or DeferTo (for [Client.Defer]).
type GuardOptions struct {
	// Action names the operation, like "stripe.transfers.create".
	Action string
	// Requester is who is asking. Empty means the client's [WithRequester].
	Requester string
	// ShowApprovers is exactly what the approvers see on the device, and
	// what the operation hash binds. It's sent as the request's params.
	ShowApprovers map[string]string
	// Summary is an optional one line description for the approvers.
	Summary string
	// IdempotencyKey, when set, makes a retried call return the same request.
	IdempotencyKey string
	// Quorum asks for a key count; policy decides whether it's allowed.
	Quorum int

	// Wait is how long to wait for a decision, at most [MaxWaitTimeout].
	Wait time.Duration
	// DeferTo hands the approved call to a worker instead of waiting.
	DeferTo *DeferTo
}

// DeferTo is the call a worker makes once the request is authorized.
type DeferTo struct {
	// Worker is the client name the worker registered, like "stripe".
	Worker string
	// Call is the dotted method on it, like "transfers.create".
	Call string
	// Args are the call's arguments, as plain JSON data.
	Args []any
	// ExecuteWithin is how long a worker may claim the call after the
	// request is created. Zero means 7 days; at most [MaxExecuteWithin].
	ExecuteWithin time.Duration
}

// Deferred is a request whose sealed call a worker runs once authorized.
// Nothing has been approved or run when [Client.Defer] returns it.
type Deferred struct {
	ID           string
	Request      *Request
	IntentDigest string
}

// WithRequester sets the requester used when [GuardOptions].Requester is empty.
func WithRequester(name string) Option {
	return func(c *Client) { c.requester = name }
}

// Guard creates a request, blocks until the approvers decide or Wait runs
// out, and returns the request only if it was authorized. Otherwise it
// returns a [*NotAuthorizedError] or a [*WaitTimeoutError]. A nil error
// means run the operation now; Guard never runs anything. Cancel ctx to stop
// waiting, which leaves the request open in Outis.
func (c *Client) Guard(ctx context.Context, opts GuardOptions) (*Request, error) {
	if opts.DeferTo != nil {
		return nil, errors.New("outis: guard: DeferTo is set; use client.Defer, which returns a *Deferred instead of waiting")
	}
	if opts.Wait == 0 {
		return nil, fmt.Errorf("outis: guard: set Wait, or set DeferTo and call client.Defer: %w", ErrTimeoutRequired)
	}
	if err := checkTimeout(opts.Wait); err != nil {
		return nil, err
	}
	var copts []CreateOption
	if opts.IdempotencyKey != "" {
		copts = append(copts, WithIdempotencyKey(opts.IdempotencyKey))
	}
	r, err := c.Requests.Create(ctx, CreateParams{
		Action:    opts.Action,
		Requester: c.requesterFor(opts),
		Params:    opts.ShowApprovers,
		Summary:   opts.Summary,
		Quorum:    opts.Quorum,
	}, copts...)
	if err != nil {
		return nil, err
	}
	if r.Live {
		if r, err = c.Requests.waitFor(ctx, r.ID, opts.Wait); err != nil {
			return nil, err
		}
	}
	if !r.IsAuthorized() {
		return nil, &NotAuthorizedError{RequestID: r.ID, Outcome: r.Outcome, Request: r}
	}
	return r, nil
}

// Defer seals opts.DeferTo under the client's intent key, creates the
// request, and returns as soon as it exists. Nothing waits and nothing runs
// here: a [Worker] makes the call once the approvers authorize it.
func (c *Client) Defer(ctx context.Context, opts GuardOptions) (*Deferred, error) {
	if opts.DeferTo == nil {
		return nil, errors.New("outis: defer: DeferTo is required; to wait instead, set Wait and call client.Guard")
	}
	if opts.Wait != 0 {
		return nil, errors.New("outis: defer: set Wait or DeferTo, not both")
	}
	if c.intentKey == nil {
		return nil, errors.New("outis: defer: no intent key; load OUTIS_INTENT_KEY with ParseIntentKey and pass it to New with WithIntentKey")
	}
	d := opts.DeferTo
	var copts []CreateOption
	if opts.IdempotencyKey != "" {
		copts = append(copts, WithIdempotencyKey(opts.IdempotencyKey))
	}
	p, err := c.Intents.Propose(ctx, IntentParams{
		Action:        opts.Action,
		Requester:     c.requesterFor(opts),
		Params:        opts.ShowApprovers,
		Summary:       opts.Summary,
		Quorum:        opts.Quorum,
		Client:        d.Worker,
		Method:        d.Call,
		Args:          d.Args,
		ExecuteWithin: d.ExecuteWithin,
	}, copts...)
	if err != nil {
		return nil, err
	}
	return &Deferred{ID: p.RequestID, Request: p.Request, IntentDigest: p.IntentDigest}, nil
}

// GuardFunc runs [Client.Guard] and then, only if the request was
// authorized, fn. It returns fn's result, or Guard's error with fn never
// called.
func GuardFunc[T any](ctx context.Context, c *Client, opts GuardOptions, fn func(context.Context) (T, error)) (T, error) {
	if _, err := c.Guard(ctx, opts); err != nil {
		var zero T
		return zero, err
	}
	return fn(ctx)
}

func (c *Client) requesterFor(opts GuardOptions) string {
	if opts.Requester != "" {
		return opts.Requester
	}
	return c.requester
}
