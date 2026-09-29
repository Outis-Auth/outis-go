package outis

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var transferShown = map[string]string{"amount": "2500000", "currency": "usd", "destination": "acct_9f2"}

func TestGuard_Validation(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("no call expected") })
	WithIntentKey(testKey)(c)
	ctx := context.Background()
	base := GuardOptions{Action: "a", Requester: "k"}
	deferTo := &DeferTo{Worker: "stripe", Call: "transfers.create"}

	if _, err := c.Guard(ctx, base); !errors.Is(err, ErrTimeoutRequired) {
		t.Errorf("guard with neither path: %v", err)
	}
	withDefer := base
	withDefer.DeferTo = deferTo
	if _, err := c.Guard(ctx, withDefer); err == nil || !strings.Contains(err.Error(), "client.Defer") {
		t.Errorf("guard with DeferTo: %v", err)
	}
	tooLong := base
	tooLong.Wait = 2 * time.Hour
	if _, err := c.Guard(ctx, tooLong); !errors.Is(err, ErrTimeoutTooLong) {
		t.Errorf("guard 2h wait: %v", err)
	}
	if _, err := c.Defer(ctx, base); err == nil {
		t.Error("defer with neither path was accepted")
	}
	both := withDefer
	both.Wait = time.Minute
	if _, err := c.Defer(ctx, both); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("defer with both paths: %v", err)
	}
	if _, err := New("k").Defer(ctx, withDefer); err == nil || !strings.Contains(err.Error(), "OUTIS_INTENT_KEY") {
		t.Errorf("defer without a key: %v", err)
	}
	noRequester := GuardOptions{Action: "a", Wait: time.Minute}
	if _, err := c.Guard(ctx, noRequester); err == nil {
		t.Error("guard with no requester anywhere was accepted")
	}
}

func TestGuard_Outcomes(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeAuthorized, OutcomeDenied, OutcomeExpired, OutcomeAborted} {
		t.Run(string(outcome), func(t *testing.T) {
			var body map[string]any
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					if r.Header.Get("Idempotency-Key") != "g1" {
						t.Errorf("Idempotency-Key = %q", r.Header.Get("Idempotency-Key"))
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					writeReq(w, http.StatusAccepted, wire("req-1", true, "", transferShown))
					return
				}
				writeReq(w, http.StatusOK, wire("req-1", false, outcome, transferShown))
			})
			WithRequester("payouts-api")(c)
			r, err := c.Guard(context.Background(), GuardOptions{
				Action: "stripe.transfers.create", ShowApprovers: transferShown,
				IdempotencyKey: "g1", Wait: 5 * time.Minute,
			})
			if body["requester"] != "payouts-api" {
				t.Errorf("requester = %v", body["requester"])
			}
			if p, _ := body["params"].(map[string]any); p["destination"] != "acct_9f2" || len(p) != 3 {
				t.Errorf("params = %v", body["params"])
			}
			if outcome == OutcomeAuthorized {
				if err != nil || !r.IsAuthorized() {
					t.Fatalf("r=%+v err=%v", r, err)
				}
				return
			}
			var na *NotAuthorizedError
			if r != nil || !errors.As(err, &na) || na.Outcome != outcome || na.RequestID != "req-1" {
				t.Fatalf("r=%v err=%v", r, err)
			}
		})
	}
}

func TestGuard_TimesOut(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeReq(w, http.StatusOK, wire("req-1", true, "", nil))
	})
	_, err := c.Guard(context.Background(), GuardOptions{Action: "a", Requester: "k", Wait: 30 * time.Second})
	var te *WaitTimeoutError
	if !errors.As(err, &te) || te.RequestID != "req-1" || te.Timeout != 30*time.Second {
		t.Fatalf("err = %v", err)
	}
}

// TestGuard_CancelStopsPolling cancels ctx mid-wait and checks that Guard
// returns the context's error and polls no more.
func TestGuard_CancelStopsPolling(t *testing.T) {
	var polls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && polls.Add(1) == 3 {
			cancel()
		}
		writeReq(w, http.StatusOK, wire("req-1", true, "", nil))
	})
	_, err := c.Guard(ctx, GuardOptions{Action: "a", Requester: "k", Wait: 5 * time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	var te *WaitTimeoutError
	if errors.As(err, &te) {
		t.Error("a cancel read as a timeout")
	}
	after := polls.Load()
	time.Sleep(20 * time.Millisecond)
	if polls.Load() != after {
		t.Error("polling continued after cancel")
	}
}

// TestGuard_Concurrent runs two guards on their own goroutines. The server
// answers neither until both have asked, so a guard that held up the other
// would fail here.
func TestGuard_Concurrent(t *testing.T) {
	var arrived sync.WaitGroup
	arrived.Add(2)
	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			arrived.Done()
			select {
			case <-both:
			case <-time.After(5 * time.Second):
				t.Error("the second guard never started")
			}
			writeReq(w, http.StatusAccepted, wire(b["action"].(string), true, "", nil))
			return
		}
		writeReq(w, http.StatusOK, wire(strings.TrimPrefix(r.URL.Path, "/v1/requests/"), false, OutcomeAuthorized, nil))
	})

	results := make(chan string, 2)
	for _, id := range []string{"req-a", "req-b"} {
		go func() {
			r, err := c.Guard(context.Background(), GuardOptions{Action: id, Requester: "k", Wait: time.Minute})
			if err != nil {
				t.Error(err)
				results <- ""
				return
			}
			results <- r.ID
		}()
	}
	got := map[string]bool{<-results: true, <-results: true}
	if !got["req-a"] || !got["req-b"] {
		t.Errorf("results = %v", got)
	}
}

func TestDefer(t *testing.T) {
	var body map[string]any
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("Defer waited: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		params := map[string]string{}
		for k, v := range body["params"].(map[string]any) {
			params[k] = v.(string)
		}
		writeReq(w, http.StatusAccepted, wire("req-d", true, "", params))
	})
	WithIntentKey(testKey)(c)
	args := map[string]any{"amount": 2500000, "currency": "usd", "destination": "acct_9f2"}
	d, err := c.Defer(context.Background(), GuardOptions{
		Action: "stripe.transfers.create", Requester: "payouts-api", ShowApprovers: transferShown,
		DeferTo: &DeferTo{Worker: "stripe", Call: "transfers.create", Args: []any{args}, ExecuteWithin: 48 * time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "req-d" || d.Request == nil || calls.Load() != 1 {
		t.Errorf("deferred = %+v, calls = %d", d, calls.Load())
	}
	params := body["params"].(map[string]any)
	if params[IntentParamKey] != d.IntentDigest || params["destination"] != "acct_9f2" {
		t.Errorf("params = %v", params)
	}
	if body["execute_within"] != float64(172800) {
		t.Errorf("execute_within = %v", body["execute_within"])
	}
	raw, _ := json.Marshal(body["intent"])
	var env IntentEnvelope
	_ = json.Unmarshal(raw, &env)
	pt, err := OpenIntent([][]byte{testKey}, "stripe.transfers.create", &env)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := EncodeIntent("stripe", "transfers.create", []any{args})
	if string(pt) != string(want) || IntentDigest(pt) != d.IntentDigest {
		t.Errorf("sealed %s, want %s", pt, want)
	}
}

func TestGuardFunc(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeAuthorized, OutcomeDenied} {
		t.Run(string(outcome), func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				writeReq(w, http.StatusOK, wire("req-1", false, outcome, nil))
			})
			ran := false
			got, err := GuardFunc(context.Background(), c, GuardOptions{Action: "a", Requester: "k", Wait: time.Minute},
				func(ctx context.Context) (string, error) {
					ran = true
					return "tr_1", nil
				})
			if outcome == OutcomeAuthorized {
				if err != nil || got != "tr_1" || !ran {
					t.Fatalf("got=%q err=%v ran=%v", got, err, ran)
				}
				return
			}
			var na *NotAuthorizedError
			if ran || got != "" || !errors.As(err, &na) {
				t.Fatalf("got=%q err=%v ran=%v", got, err, ran)
			}
		})
	}
}
