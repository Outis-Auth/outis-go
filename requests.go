package outis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// MaxWaitTimeout caps [RequestsService.WaitFor] and [Client.Guard]. Approvals
// that can take longer belong with [Client.Defer] and a [Worker].
const MaxWaitTimeout = 30 * time.Minute

const (
	pollStart = time.Second
	pollCap   = 10 * time.Second
)

// RequestsService is the request routes. Reach it as client.Requests.
type RequestsService struct {
	c *Client
}

// CreateParams is a new request. Action and Requester are required; Params
// are what the operators see and what the operation hash covers.
type CreateParams struct {
	Action    string
	Requester string
	Params    map[string]string
	// Summary is a one line description for the operators.
	Summary string
	// CallbackURL, when set, is posted the signed decision event.
	CallbackURL string
	// Quorum asks for a key count; policy decides whether it's allowed.
	Quorum int
}

type createBody struct {
	Action      string            `json:"action"`
	Requester   string            `json:"requester"`
	Params      map[string]string `json:"params"`
	Summary     string            `json:"summary,omitempty"`
	CallbackURL string            `json:"callback_url,omitempty"`
	Quorum      int               `json:"quorum,omitempty"`
	// Intent and ExecuteWithin are set only by Intents.Propose.
	Intent        *IntentEnvelope `json:"intent,omitempty"`
	ExecuteWithin int64           `json:"execute_within,omitempty"`
}

// CreateOption adjusts one [RequestsService.Create] call.
type CreateOption func(*createOptions)

type createOptions struct {
	idempotencyKey string
}

// WithIdempotencyKey sends key as the Idempotency-Key header. Creating again
// with the same key and the same operation returns the original request
// (Replayed is true); the same key with a different operation is a 409 with
// code "idempotency_conflict". With a key set, Create is also retried on
// transient failures.
func WithIdempotencyKey(key string) CreateOption {
	return func(o *createOptions) { o.idempotencyKey = key }
}

// Create asks Outis for a request and returns as soon as it exists. Nothing
// has been authorized yet.
func (s *RequestsService) Create(ctx context.Context, p CreateParams, opts ...CreateOption) (*Request, error) {
	params := p.Params
	if params == nil {
		params = map[string]string{}
	}
	return s.create(ctx, createBody{
		Action:      p.Action,
		Requester:   p.Requester,
		Params:      params,
		Summary:     p.Summary,
		CallbackURL: p.CallbackURL,
		Quorum:      p.Quorum,
	}, opts)
}

func (s *RequestsService) create(ctx context.Context, b createBody, opts []CreateOption) (*Request, error) {
	var o createOptions
	for _, fn := range opts {
		fn(&o)
	}
	if b.Action == "" {
		return nil, errors.New("outis: create: Action is required")
	}
	if b.Requester == "" {
		return nil, errors.New("outis: create: Requester is required")
	}
	if o.idempotencyKey != "" && !validIdempotencyKey(o.idempotencyKey) {
		return nil, errors.New("outis: create: an idempotency key is 1 to 255 printable ASCII characters")
	}
	body, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("outis: create: %w", err)
	}
	header := http.Header{}
	if o.idempotencyKey != "" {
		header.Set("Idempotency-Key", o.idempotencyKey)
	}
	r, hdr, err := s.c.send(ctx, http.MethodPost, "/v1/requests", body, header, o.idempotencyKey != "")
	if err != nil {
		return nil, err
	}
	r.Replayed = hdr.Get("Idempotent-Replayed") == "true"
	return r, nil
}

// Retrieve reads a request's current state.
func (s *RequestsService) Retrieve(ctx context.Context, id string) (*Request, error) {
	if id == "" {
		return nil, errors.New("outis: retrieve: id is required")
	}
	r, _, err := s.c.send(ctx, http.MethodGet, "/v1/requests/"+url.PathEscape(id), nil, nil, true)
	return r, err
}

// WaitFor polls until the request is decided or timeout passes, backing off
// from one second to ten. It returns the request on any outcome, authorized
// or not, and a [*WaitTimeoutError] carrying the id when time runs out.
// Timeout is required and at most [MaxWaitTimeout].
func (s *RequestsService) WaitFor(ctx context.Context, id string, timeout time.Duration) (*Request, error) {
	if err := checkTimeout(timeout); err != nil {
		return nil, err
	}
	return s.waitFor(ctx, id, timeout)
}

func (s *RequestsService) waitFor(ctx context.Context, id string, timeout time.Duration) (*Request, error) {
	c := s.c
	deadline := c.clock.Now().Add(timeout)
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last *Request
	timedOut := func() error {
		return &WaitTimeoutError{RequestID: id, Request: last, Timeout: timeout}
	}
	delay := pollStart
	for {
		r, err := s.Retrieve(wctx, id)
		if err != nil {
			if ctx.Err() == nil && wctx.Err() != nil {
				return nil, timedOut()
			}
			return nil, err
		}
		last = r
		if !r.Live {
			return r, nil
		}
		remaining := deadline.Sub(c.clock.Now())
		if remaining <= 0 {
			return nil, timedOut()
		}
		if err := c.clock.Sleep(wctx, min(c.jitter(delay), remaining)); err != nil {
			if ctx.Err() == nil {
				return nil, timedOut()
			}
			return nil, err
		}
		delay = min(delay*2, pollCap)
	}
}

// AssertAuthorized is the executor's check before it runs anything: it reads
// the request and requires that it was authorized, and that what was
// authorized is exactly op. It returns a [*NotAuthorizedError] or an
// [*OperationMismatchError] otherwise.
func (s *RequestsService) AssertAuthorized(ctx context.Context, id string, op Operation) (*Request, error) {
	r, err := s.Retrieve(ctx, id)
	if err != nil {
		return nil, err
	}
	if !r.IsAuthorized() {
		return nil, &NotAuthorizedError{RequestID: r.ID, Outcome: r.Outcome, Request: r}
	}
	want := op.Hash()
	got := r.OperationHash
	if got == "" {
		got = r.Operation().Hash()
	}
	if got != want {
		return nil, &OperationMismatchError{RequestID: r.ID, Expected: want, Actual: got, Request: r}
	}
	return r, nil
}

func checkTimeout(d time.Duration) error {
	if d <= 0 {
		return ErrTimeoutRequired
	}
	if d > MaxWaitTimeout {
		return fmt.Errorf("%w: %s is more than %s. For approvals that take longer, create the request, store its id, and resume from a webhook or a later Retrieve", ErrTimeoutTooLong, d, MaxWaitTimeout)
	}
	return nil
}

func validIdempotencyKey(k string) bool {
	if len(k) < 1 || len(k) > 255 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return false
		}
	}
	return true
}
