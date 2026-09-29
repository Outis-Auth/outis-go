package outis

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries a delivery's timestamp and signatures.
const SignatureHeader = "Outis-Signature"

// DefaultWebhookTolerance is how far a delivery's timestamp may be from now.
const DefaultWebhookTolerance = 5 * time.Minute

// The event types Outis delivers.
const (
	EventRequestAuthorized = "request.authorized"
	EventRequestDenied     = "request.denied"
	EventRequestExpired    = "request.expired"
	EventRequestAborted    = "request.aborted"
	EventRequestFailed     = "request.failed"
	// A worker reported its run of the request's intent.
	EventRequestExecuted        = "request.executed"
	EventRequestExecutionFailed = "request.execution_failed"
)

// Event is a verified webhook delivery. Deliveries can repeat, so dedupe on ID.
type Event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Org       string    `json:"org"`
	Data      EventData `json:"data"`
}

// EventData is an event's payload.
type EventData struct {
	Request Request `json:"request"`
}

// VerifyOptions adjusts [VerifyWebhook]. The zero value uses the defaults.
type VerifyOptions struct {
	// Tolerance is how far the signed timestamp may be from now. Zero means
	// [DefaultWebhookTolerance].
	Tolerance time.Duration
	// Now is the current time. Nil means time.Now.
	Now func() time.Time
}

// VerifyWebhook checks a delivery's Outis-Signature against an endpoint's
// secret (the whsec_ string, used as is) and returns the parsed event. Pass
// the raw body exactly as received, before any JSON decoding. It returns a
// [*WebhookVerificationError] when the signature is missing, wrong or stale.
// Any one matching v1 signature passes, which is how a delivery stays valid
// across a secret rotation.
func VerifyWebhook(body []byte, header http.Header, secret string, opts *VerifyOptions) (*Event, error) {
	if secret == "" {
		return nil, &WebhookVerificationError{Reason: "no secret given"}
	}
	return VerifyWebhookKeys(body, header, [][]byte{[]byte(secret)}, opts)
}

// VerifyWebhookKeys is [VerifyWebhook] with raw HMAC keys, any of which may
// match. Use it for a request's callback_url, whose key is
// [CallbackSecret], or to accept an endpoint secret and a callback key on
// one receiver.
func VerifyWebhookKeys(body []byte, header http.Header, keys [][]byte, opts *VerifyOptions) (*Event, error) {
	tolerance := DefaultWebhookTolerance
	now := time.Now
	if opts != nil {
		if opts.Tolerance > 0 {
			tolerance = opts.Tolerance
		}
		if opts.Now != nil {
			now = opts.Now
		}
	}
	fail := func(reason string) (*Event, error) {
		return nil, &WebhookVerificationError{Reason: reason}
	}
	keys = slices.DeleteFunc(slices.Clone(keys), func(k []byte) bool { return len(k) == 0 })
	if len(keys) == 0 {
		return fail("no secret given")
	}

	sig := header.Get(SignatureHeader)
	if sig == "" {
		return fail("no " + SignatureHeader + " header")
	}
	var ts string
	var sigs [][]byte
	for _, part := range strings.Split(sig, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			if b, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, b)
			}
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fail("no timestamp in " + SignatureHeader)
	}
	if len(sigs) == 0 {
		return fail("no v1 signature in " + SignatureHeader)
	}

	matched := false
	for _, key := range keys {
		want := signature(key, ts, body)
		for _, s := range sigs {
			if hmac.Equal(s, want) {
				matched = true
			}
		}
	}
	if !matched {
		return fail("signature doesn't match")
	}
	if skew := math.Abs(float64(now().Unix() - t)); skew > tolerance.Seconds() {
		return fail("timestamp is outside the tolerance")
	}

	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return fail("body isn't an event: " + err.Error())
	}
	return &ev, nil
}

// SignWebhook computes an Outis-Signature header value for body at t under an
// endpoint secret. It's for testing a receiver; Outis signs real deliveries.
func SignWebhook(body []byte, secret string, t time.Time) string {
	return SignWebhookKey(body, []byte(secret), t)
}

// SignWebhookKey is [SignWebhook] with a raw HMAC key, such as [CallbackSecret].
func SignWebhookKey(body []byte, key []byte, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(signature(key, ts, body))
}

func signature(key []byte, ts string, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return mac.Sum(nil)
}

// callbackKeyInfo is the message the callback key is derived from.
const callbackKeyInfo = "outis/callback-key/v1"

// CallbackSecret is the key Outis signs a request's callback_url deliveries
// with: HMAC-SHA256 keyed by the API key that created the request, over
// "outis/callback-key/v1". Pass it to [VerifyWebhookKeys].
func CallbackSecret(apiKey string) []byte {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte(callbackKeyInfo))
	return mac.Sum(nil)
}
