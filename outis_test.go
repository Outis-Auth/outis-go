package outis

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// fakeClock advances only when slept on, and records every sleep.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	f.sleeps = append(f.sleeps, d)
	return nil
}

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *fakeClock) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("outis_sk_test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	fc := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	c.clock = fc
	c.jitter = func(d time.Duration) time.Duration { return d }
	return c, fc
}

func wire(id string, live bool, outcome Outcome, params map[string]string) map[string]any {
	r := map[string]any{
		"id": id, "action": "deploy.production", "requester": "keith",
		"state": "notified", "live": live, "outcome": nil,
		"approvers": []string{}, "params": params,
		"operation_hash": OperationHash("deploy.production", params),
		"created_at":     1788350100000, "decided_at": nil,
	}
	if outcome != "" {
		r["outcome"] = string(outcome)
		r["state"] = string(outcome)
		r["decided_at"] = 1788350400000
		if outcome == OutcomeAuthorized {
			r["state"] = "succeeded"
			r["approvers"] = []string{"maya", "sam"}
		}
	}
	return r
}

func writeReq(w http.ResponseWriter, status int, r map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"server_now": 1788350400000, "request": r})
}

func writeErr(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

var deployParams = map[string]string{"repo": "acme/payments-api", "env": "production", "sha": "8d93f71"}

func TestOperationHashVectors(t *testing.T) {
	raw, err := os.ReadFile("vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		OperationHash []struct {
			Action string            `json:"action"`
			Params map[string]string `json:"params"`
			Hash   string            `json:"hash"`
		} `json:"operation_hash"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.OperationHash) != 3 {
		t.Fatalf("want 3 vectors, got %d", len(v.OperationHash))
	}
	for _, c := range v.OperationHash {
		if got := OperationHash(c.Action, c.Params); got != c.Hash {
			t.Errorf("OperationHash(%q, %v) = %s, want %s", c.Action, c.Params, got, c.Hash)
		}
	}
	if OperationHash("deploy.production", nil) != "sha256:da61379e121b0a00280eba6ee7d631e3c0c186e7da15ade5b8d15c901d0df8a9" {
		t.Error("nil params must hash like empty params")
	}
}

func TestCreate(t *testing.T) {
	var got struct {
		body   map[string]any
		header http.Header
	}
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/requests" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		got.header = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		writeReq(w, http.StatusAccepted, wire("req-1", true, "", deployParams))
	})

	r, err := c.Requests.Create(context.Background(), CreateParams{
		Action: "deploy.production", Requester: "keith", Params: deployParams, Summary: "ship it",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.header.Get("Authorization") != "Bearer outis_sk_test" {
		t.Errorf("Authorization = %q", got.header.Get("Authorization"))
	}
	if _, ok := got.header["Idempotency-Key"]; ok {
		t.Error("Idempotency-Key sent without being asked for")
	}
	if got.body["action"] != "deploy.production" || got.body["requester"] != "keith" || got.body["summary"] != "ship it" {
		t.Errorf("body = %v", got.body)
	}
	if _, ok := got.body["callback_url"]; ok {
		t.Error("empty callback_url should be omitted")
	}
	if r.ID != "req-1" || !r.IsPending() || r.IsAuthorized() || r.Outcome != "" || r.Replayed {
		t.Errorf("request = %+v", r)
	}
	if r.OperationHash != OperationHash("deploy.production", deployParams) {
		t.Errorf("OperationHash = %s", r.OperationHash)
	}
	if !r.CreatedAt.Equal(time.UnixMilli(1788350100000)) || !r.DecidedAt.IsZero() {
		t.Errorf("times = %v, %v", r.CreatedAt, r.DecidedAt)
	}
}

func TestCreate_IdempotencyKeyAndReplay(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "deploy-8d93f71" {
			t.Errorf("Idempotency-Key = %q", r.Header.Get("Idempotency-Key"))
		}
		w.Header().Set("Idempotent-Replayed", "true")
		writeReq(w, http.StatusAccepted, wire("req-1", true, "", deployParams))
	})
	r, err := c.Requests.Create(context.Background(), CreateParams{Action: "deploy.production", Requester: "keith", Params: deployParams},
		WithIdempotencyKey("deploy-8d93f71"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Replayed {
		t.Error("Replayed should be true when the server says so")
	}
}

func TestCreate_IdempotencyConflict(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeErr(w, http.StatusConflict, `{"error":"that key was used for a different operation","kind":"idempotency_conflict"}`)
	})
	_, err := c.Requests.Create(context.Background(), CreateParams{Action: "a", Requester: "k"}, WithIdempotencyKey("k1"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 || apiErr.Code != "idempotency_conflict" {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Errorf("a 409 was retried: %d calls", calls)
	}
}

func TestCreate_RetriesOnlyWithKey(t *testing.T) {
	for _, withKey := range []bool{false, true} {
		calls := 0
		c, fc := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				writeErr(w, http.StatusServiceUnavailable, `{"error":"busy"}`)
				return
			}
			writeReq(w, http.StatusAccepted, wire("req-1", true, "", nil))
		})
		var opts []CreateOption
		if withKey {
			opts = append(opts, WithIdempotencyKey("k1"))
		}
		_, err := c.Requests.Create(context.Background(), CreateParams{Action: "a", Requester: "k"}, opts...)
		if withKey && (err != nil || calls != 2 || len(fc.sleeps) != 1) {
			t.Errorf("with key: err=%v calls=%d sleeps=%v", err, calls, fc.sleeps)
		}
		if !withKey && (err == nil || calls != 1) {
			t.Errorf("without key: err=%v calls=%d", err, calls)
		}
	}
}

func TestCreate_Validation(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("no call expected") })
	ctx := context.Background()
	if _, err := c.Requests.Create(ctx, CreateParams{Requester: "k"}); err == nil {
		t.Error("missing action accepted")
	}
	if _, err := c.Requests.Create(ctx, CreateParams{Action: "a", Requester: "k"}, WithIdempotencyKey("bad\nkey")); err == nil {
		t.Error("non-printable idempotency key accepted")
	}
}

func TestRetrieve_RetriesAndErrors(t *testing.T) {
	calls := 0
	c, fc := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/v1/requests/req-1":
			if calls == 1 {
				w.Header().Set("Retry-After", "3")
				writeErr(w, http.StatusTooManyRequests, `{"error":"slow down"}`)
				return
			}
			writeReq(w, http.StatusOK, wire("req-1", false, OutcomeAuthorized, deployParams))
		default:
			writeErr(w, http.StatusNotFound, `{"error":"no such request","kind":"not_found"}`)
		}
	})
	r, err := c.Requests.Retrieve(context.Background(), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsAuthorized() || r.IsPending() || len(r.Approvers) != 2 || r.DecidedAt.IsZero() {
		t.Errorf("request = %+v", r)
	}
	if len(fc.sleeps) != 1 || fc.sleeps[0] != 3*time.Second {
		t.Errorf("Retry-After not honoured: %v", fc.sleeps)
	}

	_, err = c.Requests.Retrieve(context.Background(), "req-missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || apiErr.Code != "not_found" {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitFor_Resolves(t *testing.T) {
	calls := 0
	c, fc := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 5 {
			writeReq(w, http.StatusOK, wire("req-1", true, "", nil))
			return
		}
		writeReq(w, http.StatusOK, wire("req-1", false, OutcomeDenied, nil))
	})
	r, err := c.Requests.WaitFor(context.Background(), "req-1", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != OutcomeDenied {
		t.Errorf("Outcome = %s", r.Outcome)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(fc.sleeps) != len(want) {
		t.Fatalf("sleeps = %v", fc.sleeps)
	}
	for i := range want {
		if fc.sleeps[i] != want[i] {
			t.Errorf("sleeps = %v, want %v", fc.sleeps, want)
		}
	}
}

func TestWaitFor_TimesOut(t *testing.T) {
	c, fc := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeReq(w, http.StatusOK, wire("req-1", true, "", nil))
	})
	_, err := c.Requests.WaitFor(context.Background(), "req-1", time.Minute)
	var te *WaitTimeoutError
	if !errors.As(err, &te) || te.RequestID != "req-1" || te.Request == nil || te.Timeout != time.Minute {
		t.Fatalf("err = %v", err)
	}
	var total time.Duration
	for _, d := range fc.sleeps {
		if d > 10*time.Second {
			t.Errorf("sleep %s over the cap", d)
		}
		total += d
	}
	if total != time.Minute {
		t.Errorf("slept %s, want exactly the timeout", total)
	}
}

func TestWaitFor_TimeoutRules(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("no call expected") })
	ctx := context.Background()
	if _, err := c.Requests.WaitFor(ctx, "req-1", 0); !errors.Is(err, ErrTimeoutRequired) {
		t.Errorf("zero timeout: %v", err)
	}
	if _, err := c.Requests.WaitFor(ctx, "req-1", 31*time.Minute); !errors.Is(err, ErrTimeoutTooLong) {
		t.Errorf("31m timeout: %v", err)
	}
}

func TestWaitFor_Cancelled(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeReq(w, http.StatusOK, wire("req-1", true, "", nil))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Requests.WaitFor(ctx, "req-1", time.Minute)
	var te *WaitTimeoutError
	if err == nil || errors.As(err, &te) {
		t.Fatalf("a cancelled context should not read as a timeout: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestAssertAuthorized(t *testing.T) {
	outcome := OutcomeAuthorized
	live := false
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeReq(w, http.StatusOK, wire("req-1", live, outcome, deployParams))
	})
	ctx := context.Background()
	op := Operation{Action: "deploy.production", Params: deployParams}

	if _, err := c.Requests.AssertAuthorized(ctx, "req-1", op); err != nil {
		t.Fatalf("matching operation refused: %v", err)
	}

	moved := Operation{Action: "deploy.production", Params: map[string]string{"repo": "acme/payments-api", "env": "production", "sha": "ffffff0"}}
	_, err := c.Requests.AssertAuthorized(ctx, "req-1", moved)
	var mm *OperationMismatchError
	if !errors.As(err, &mm) || mm.Expected != moved.Hash() || mm.Actual != op.Hash() {
		t.Fatalf("err = %v", err)
	}

	outcome, live = "", true
	_, err = c.Requests.AssertAuthorized(ctx, "req-1", op)
	var na *NotAuthorizedError
	if !errors.As(err, &na) || na.Outcome != "" {
		t.Fatalf("pending request: %v", err)
	}
}

func TestRequestJSONRoundTrip(t *testing.T) {
	in := wire("req-1", false, OutcomeAuthorized, deployParams)
	raw, _ := json.Marshal(in)
	var r Request
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(out, &b)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("round trip changed the body:\n%s\n%s", ja, jb)
	}
}
