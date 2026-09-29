package outis

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"
)

// MaxExecuteWithin caps [IntentParams.ExecuteWithin].
const MaxExecuteWithin = 30 * 24 * time.Hour

// IntentsService proposes sealed calls. Reach it as client.Intents; it needs
// a key from [WithIntentKey].
type IntentsService struct {
	c *Client
}

// WithIntentKey sets the 32 byte key intents are sealed under (see
// [ParseIntentKey]). Only your workers hold it; Outis never sees it.
func WithIntentKey(key []byte) Option {
	return func(c *Client) { c.intentKey = key }
}

// IntentParams is a request whose authorized call a worker runs later.
// Client names the client the worker registered, Method a dotted path on it
// (or a name given to [Worker.Handle]), and Args its JSON arguments.
type IntentParams struct {
	Action    string
	Requester string
	// Params are what the operators see. Outis adds the intent's digest
	// under "intent", so don't set that key yourself.
	Params      map[string]string
	Summary     string
	CallbackURL string
	Quorum      int

	Client string
	Method string
	Args   []any
	// ExecuteWithin is how long after creation a worker may claim the call.
	// Zero means the server's default of 7 days; at most [MaxExecuteWithin].
	ExecuteWithin time.Duration
}

// PendingIntent is a proposed call. Nothing runs until the request is
// authorized and a worker claims it.
type PendingIntent struct {
	RequestID    string
	Request      *Request
	IntentDigest string
}

// Propose seals the call under the client's intent key, binds its digest
// into the params, and creates the request. The call isn't made here.
func (s *IntentsService) Propose(ctx context.Context, p IntentParams, opts ...CreateOption) (*PendingIntent, error) {
	if s.c.intentKey == nil {
		return nil, errors.New("outis: propose: no intent key; set one with WithIntentKey")
	}
	if _, ok := p.Params[IntentParamKey]; ok {
		return nil, fmt.Errorf("outis: propose: params can't carry %q, Outis sets it", IntentParamKey)
	}
	if p.ExecuteWithin < 0 || p.ExecuteWithin > MaxExecuteWithin {
		return nil, fmt.Errorf("outis: propose: ExecuteWithin is at most %s", MaxExecuteWithin)
	}
	if p.ExecuteWithin > 0 && p.ExecuteWithin < time.Second {
		return nil, errors.New("outis: propose: ExecuteWithin is at least a second")
	}
	if p.Args == nil {
		p.Args = []any{}
	}
	plaintext, err := EncodeIntent(p.Client, p.Method, p.Args)
	if err != nil {
		return nil, err
	}
	env, err := SealIntent(s.c.intentKey, p.Action, plaintext)
	if err != nil {
		return nil, err
	}
	digest := IntentDigest(plaintext)
	params := maps.Clone(p.Params)
	if params == nil {
		params = map[string]string{}
	}
	params[IntentParamKey] = digest

	r, err := s.c.Requests.create(ctx, createBody{
		Action:        p.Action,
		Requester:     p.Requester,
		Params:        params,
		Summary:       p.Summary,
		CallbackURL:   p.CallbackURL,
		Quorum:        p.Quorum,
		Intent:        env,
		ExecuteWithin: int64(p.ExecuteWithin / time.Second),
	}, opts)
	if err != nil {
		return nil, err
	}
	return &PendingIntent{RequestID: r.ID, Request: r, IntentDigest: digest}, nil
}
