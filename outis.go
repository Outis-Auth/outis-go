// Package outis is the Go client for Outis, the approval gate that asks the
// required people before an operation runs. Outis authorizes; your code
// executes, with its own credentials.
//
// [Client.Guard] waits for approval and returns; [Client.Defer] hands the
// approved call to a [Worker]. [Client.Requests] is the full request API.
package outis

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// Version is this client's version, sent in the User-Agent header.
const Version = "0.1.0"

// DefaultBaseURL is the Outis API.
const DefaultBaseURL = "https://api.outis.tech"

// Client talks to the Outis API with one API key.
type Client struct {
	// Requests creates, reads and waits on requests.
	Requests *RequestsService
	// Intents proposes sealed calls for a worker to run once authorized.
	Intents *IntentsService

	intentKey []byte
	requester string
	apiKey    string
	baseURL   string
	http      *http.Client
	clock     clock
	jitter    func(time.Duration) time.Duration
}

// Option configures a [Client].
type Option func(*Client)

// WithBaseURL points the client at another Outis deployment.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient replaces the HTTP client, for proxies, custom transports or tests.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New returns a client using apiKey, an Outis service token (outis_sk_...).
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:  apiKey,
		baseURL: DefaultBaseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
		clock:   realClock{},
		jitter:  defaultJitter,
	}
	for _, o := range opts {
		o(c)
	}
	c.Requests = &RequestsService{c: c}
	c.Intents = &IntentsService{c: c}
	return c
}

// clock is the time source for waits and retry backoff, replaced in tests.
type clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// defaultJitter spreads d over [0.8d, 1.2d) so many waiters don't poll in step.
func defaultJitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}
