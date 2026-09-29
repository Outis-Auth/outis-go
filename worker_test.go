package outis

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var testKey = bytes.Repeat([]byte{7}, 32)

// Stand-ins for a Stripe-like client: a field holding a service whose
// params take an idempotency key through an embedded struct.
type fakeParams struct {
	IdempotencyKey *string `json:"-"`
}

func (p *fakeParams) SetIdempotencyKey(k string) { p.IdempotencyKey = &k }

type transferParams struct {
	fakeParams
	Amount      int64  `json:"amount"`
	Destination string `json:"destination"`
}

type transfer struct {
	ID     string
	Amount int64
}

type transfersService struct {
	mu    sync.Mutex
	calls []*transferParams
	keys  []string
	fail  error
}

func (s *transfersService) Create(ctx context.Context, p *transferParams) (*transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, p)
	s.keys = append(s.keys, IdempotencyKey(ctx))
	if s.fail != nil {
		return nil, s.fail
	}
	return &transfer{ID: "tr_" + p.Destination, Amount: p.Amount}, nil
}

type fakeStripe struct {
	V1Transfers *transfersService
	secret      string
}

type calc struct{}

func (calc) Sum(xs ...int) int { return len(xs) }

// fakeExec is an Outis that serves list, claim and execution routes over a
// set of requests.
type fakeExec struct {
	t        *testing.T
	mu       sync.Mutex
	reqs     map[string]*Request
	order    []string
	claims   map[string]int
	reports  map[string][]map[string]any
	created  map[string]any
	conflict map[string]string
}

func newFakeExec(t *testing.T) (*fakeExec, *Client) {
	f := &fakeExec{
		t: t, reqs: map[string]*Request{}, claims: map[string]int{},
		reports: map[string][]map[string]any{}, conflict: map[string]string{},
	}
	c, _ := newTestClient(t, f.serve)
	return f, c
}

func (f *fakeExec) add(r *Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs[r.ID] = r
	f.order = append(f.order, r.ID)
}

func (f *fakeExec) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/requests"), "/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/requests":
		_ = json.NewDecoder(r.Body).Decode(&f.created)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"request": map[string]any{"id": "req-new", "action": f.created["action"], "live": true}})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/requests":
		if r.URL.Query().Get("executable") != "true" {
			f.t.Errorf("list without executable=true: %s", r.URL.RawQuery)
		}
		var out []*Request
		for _, id := range f.order {
			if f.claims[id] == 0 {
				out = append(out, f.reqs[id])
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": out})
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "claim":
		id := parts[1]
		if kind := f.conflict[id]; kind != "" {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"no","kind":"`+kind+`"}`)
			return
		}
		f.claims[id]++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"claim_id": "clm-" + id, "lease_expires_at": 1800000600000, "request": f.reqs[id],
		})
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "execution":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.reports[parts[1]] = append(f.reports[parts[1]], body)
		_, _ = io.WriteString(w, `{}`)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeExec) report(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reports[id]) != 1 {
		f.t.Fatalf("%s: want one report, got %v", id, f.reports[id])
	}
	return f.reports[id][0]
}

// authorized builds an authorized request whose intent is sealed under key.
func authorized(t *testing.T, id, action, client, method string, args ...any) *Request {
	t.Helper()
	pt, err := EncodeIntent(client, method, args)
	if err != nil {
		t.Fatal(err)
	}
	env, err := SealIntent(testKey, action, pt)
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"to": "acct_9f2", IntentParamKey: IntentDigest(pt)}
	return &Request{
		ID: id, Action: action, Requester: "payouts", State: StateSucceeded, Outcome: OutcomeAuthorized,
		Params: params, OperationHash: OperationHash(action, params), Intent: env,
		Execution: ExecutionRecord{State: ExecutionPending},
	}
}

func transferArgs() map[string]any {
	return map[string]any{"amount": 25000000, "destination": "acct_9f2"}
}

func TestIntentSealOpen(t *testing.T) {
	pt, err := EncodeIntent("stripe", "transfers.create", []any{map[string]any{"amount": 25000000, "note": "<a&b>"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":1,"client":"stripe","method":"transfers.create","args":[{"amount":25000000,"note":"<a&b>"}]}`; string(pt) != want {
		t.Fatalf("plaintext = %s", pt)
	}
	env, err := SealIntent(testKey, "stripe.transfer", pt)
	if err != nil {
		t.Fatal(err)
	}
	if env.KeyID != IntentKeyID(testKey) || len(env.KeyID) != 16 || env.Alg != "A256GCM" || env.V != 1 {
		t.Errorf("envelope = %+v", env)
	}
	got, err := OpenIntent([][]byte{bytes.Repeat([]byte{1}, 32), testKey}, "stripe.transfer", env)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("open = %s, %v", got, err)
	}

	var ie *IntentError
	if _, err := OpenIntent([][]byte{testKey}, "stripe.payout", env); !errors.As(err, &ie) || ie.Reason != ReasonDecryptFailed {
		t.Errorf("another action opened the intent: %v", err)
	}
	if _, err := OpenIntent([][]byte{bytes.Repeat([]byte{1}, 32)}, "stripe.transfer", env); !errors.As(err, &ie) || ie.Reason != ReasonUnknownKey {
		t.Errorf("unknown kid: %v", err)
	}
	if _, err := EncodeIntent("c", "m", []any{func() {}}); err == nil {
		t.Error("a func arg was encoded")
	}
}

func TestParseIntentKey(t *testing.T) {
	for _, s := range []string{
		"BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=",
		"BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc",
	} {
		k, err := ParseIntentKey(s)
		if err != nil || !bytes.Equal(k, testKey) {
			t.Errorf("ParseIntentKey(%q) = %x, %v", s, k, err)
		}
	}
	if _, err := ParseIntentKey("c2hvcnQ="); err == nil {
		t.Error("a short key was accepted")
	}
	keys, err := ParseIntentKeys("BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=, AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	if err != nil || len(keys) != 2 {
		t.Errorf("ParseIntentKeys = %d keys, %v", len(keys), err)
	}
	gen, _ := GenerateIntentKey()
	if _, err := ParseIntentKey(gen); err != nil {
		t.Errorf("a generated key doesn't parse: %v", err)
	}
}

func TestPropose(t *testing.T) {
	f, c := newFakeExec(t)
	WithIntentKey(testKey)(c)
	p, err := c.Intents.Propose(context.Background(), IntentParams{
		Action: "stripe.transfer", Requester: "payouts", Params: map[string]string{"to": "acct_9f2"},
		Client: "stripe", Method: "transfers.create", Args: []any{transferArgs()},
		ExecuteWithin: 48 * time.Hour,
	}, WithIdempotencyKey("payout-1"))
	if err != nil {
		t.Fatal(err)
	}
	if p.RequestID != "req-new" {
		t.Errorf("RequestID = %s", p.RequestID)
	}
	params := f.created["params"].(map[string]any)
	if params["intent"] != p.IntentDigest || params["to"] != "acct_9f2" {
		t.Errorf("params = %v", params)
	}
	if f.created["execute_within"] != float64(172800) {
		t.Errorf("execute_within = %v", f.created["execute_within"])
	}
	raw, _ := json.Marshal(f.created["intent"])
	var env IntentEnvelope
	_ = json.Unmarshal(raw, &env)
	pt, err := OpenIntent([][]byte{testKey}, "stripe.transfer", &env)
	if err != nil {
		t.Fatal(err)
	}
	if IntentDigest(pt) != p.IntentDigest || !strings.Contains(string(pt), `"method":"transfers.create"`) {
		t.Errorf("sealed %s", pt)
	}

	if _, err := c.Intents.Propose(context.Background(), IntentParams{
		Action: "a", Requester: "r", Params: map[string]string{"intent": "x"}, Client: "c", Method: "m",
	}); err == nil {
		t.Error("params carrying intent were accepted")
	}
	bare := New("k")
	if _, err := bare.Intents.Propose(context.Background(), IntentParams{Action: "a", Requester: "r", Client: "c", Method: "m"}); err == nil {
		t.Error("propose without a key succeeded")
	}
}

func newStripeWorker(c *Client, opts WorkerOptions) (*Worker, *transfersService) {
	svc := &transfersService{}
	if opts.Keys == nil {
		opts.Keys = [][]byte{testKey}
	}
	w := NewWorker(c, opts)
	w.Register("stripe", &fakeStripe{V1Transfers: svc, secret: "sk_live"}, InjectIdempotencyKey())
	return w, svc
}

func TestWorkerExecute_RegisteredClient(t *testing.T) {
	f, c := newFakeExec(t)
	f.add(authorized(t, "req-1", "stripe.transfer", "stripe", "v1_transfers.create", transferArgs()))
	var seen []Result
	w, svc := newStripeWorker(c, WorkerOptions{OnResult: func(r Result) { seen = append(seen, r) }})

	res, err := w.Execute(context.Background(), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ExecutionSucceeded || res.Reference != "tr_acct_9f2" || res.Err != nil {
		t.Errorf("result = %+v", res)
	}
	if len(svc.calls) != 1 || svc.calls[0].Amount != 25000000 {
		t.Fatalf("calls = %+v", svc.calls)
	}
	if k := svc.calls[0].IdempotencyKey; k == nil || *k != "req-1" || svc.keys[0] != "req-1" {
		t.Errorf("idempotency key not passed: %v, %v", k, svc.keys)
	}
	rep := f.report("req-1")
	if rep["claim_id"] != "clm-req-1" || rep["status"] != "succeeded" || rep["reference"] != "tr_acct_9f2" {
		t.Errorf("report = %v", rep)
	}
	if len(seen) != 1 {
		t.Errorf("OnResult saw %d results", len(seen))
	}
}

func TestWorkerExecute_KeyOnlyWhereOptedIn(t *testing.T) {
	f, c := newFakeExec(t)
	f.add(authorized(t, "req-1", "stripe.transfer", "stripe", "V1Transfers.Create", transferArgs()))
	svc := &transfersService{}
	w := NewWorker(c, WorkerOptions{Keys: [][]byte{testKey}})
	w.Register("stripe", &fakeStripe{V1Transfers: svc})
	if _, err := w.Execute(context.Background(), "req-1"); err != nil {
		t.Fatal(err)
	}
	if svc.calls[0].IdempotencyKey != nil {
		t.Error("key set on a client that didn't opt in")
	}
	if svc.keys[0] != "req-1" {
		t.Error("IdempotencyKey(ctx) should be available to every call")
	}
}

func TestWorkerExecute_Handle(t *testing.T) {
	f, c := newFakeExec(t)
	f.add(authorized(t, "req-1", "db.restore", "db", "restore", "payments", "2026-09-27T04:00Z"))
	w := NewWorker(c, WorkerOptions{Keys: [][]byte{testKey}})
	var gotArgs []string
	var gotX Execution
	w.Handle("db.restore", func(ctx context.Context, x Execution, args json.RawMessage) (string, error) {
		gotX = x
		if IdempotencyKey(ctx) != "req-1" {
			t.Errorf("IdempotencyKey(ctx) = %q", IdempotencyKey(ctx))
		}
		return "restore-7", json.Unmarshal(args, &gotArgs)
	})
	res, err := w.Execute(context.Background(), "req-1")
	if err != nil || res.Status != ExecutionSucceeded || res.Reference != "restore-7" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "payments" || gotX.ClaimID != "clm-req-1" || gotX.IdempotencyKey != "req-1" {
		t.Errorf("handler saw %v, %+v", gotArgs, gotX)
	}
	if f.report("req-1")["reference"] != "restore-7" {
		t.Error("reference not reported")
	}
}

func TestWorkerExecute_RefusesAndFails(t *testing.T) {
	boom := errors.New("card declined")
	cases := []struct {
		name   string
		reason string
		mutate func(*Request)
		opts   WorkerOptions
		fail   error
	}{
		{name: "unknown key", reason: ReasonUnknownKey, opts: WorkerOptions{Keys: [][]byte{bytes.Repeat([]byte{1}, 32)}}},
		{name: "no intent", reason: ReasonNoIntent, mutate: func(r *Request) { r.Intent = nil }},
		{name: "digest", reason: ReasonDigestMismatch, mutate: func(r *Request) {
			r.Params[IntentParamKey] = "sha256:" + strings.Repeat("0", 64)
			r.OperationHash = OperationHash(r.Action, r.Params)
		}},
		{name: "operation", reason: ReasonOperationMismatch, mutate: func(r *Request) { r.Params["to"] = "acct_evil" }},
		{name: "not authorized", reason: ReasonNotAuthorized, mutate: func(r *Request) { r.Outcome = OutcomeDenied }},
		{name: "action swapped", reason: ReasonDecryptFailed, mutate: func(r *Request) {
			r.Action = "stripe.payout"
			r.OperationHash = OperationHash(r.Action, r.Params)
		}},
		{name: "not allowed", reason: ReasonNotAllowed, opts: WorkerOptions{Allow: []string{"stripe.refunds.*"}}},
		{name: "downstream error", reason: ReasonExecutionError, fail: boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeExec(t)
			r := authorized(t, "req-1", "stripe.transfer", "stripe", "v1transfers.create", transferArgs())
			if tc.mutate != nil {
				tc.mutate(r)
			}
			f.add(r)
			w, svc := newStripeWorker(c, tc.opts)
			svc.fail = tc.fail
			res, err := w.Execute(context.Background(), "req-1")
			if err != nil {
				t.Fatal(err)
			}
			var ie *IntentError
			if res.Status != ExecutionFailed || !errors.As(res.Err, &ie) || ie.Reason != tc.reason {
				t.Fatalf("result = %+v", res)
			}
			if tc.fail == nil && len(svc.calls) != 0 {
				t.Error("the call ran despite a failed check")
			}
			if tc.fail != nil && !errors.Is(res.Err, boom) {
				t.Errorf("err = %v", res.Err)
			}
			rep := f.report("req-1")
			if rep["status"] != "failed" || !strings.HasPrefix(rep["error"].(string), tc.reason) {
				t.Errorf("report = %v", rep)
			}
		})
	}
}

func TestWorkerExecute_NotRegisteredAndBadArgs(t *testing.T) {
	for name, r := range map[string]*Request{
		ReasonNotRegistered: authorized(t, "req-1", "a.b", "ledger", "post"),
		ReasonBadArgs:       authorized(t, "req-1", "a.b", "stripe", "v1transfers.create", "not an object"),
	} {
		f, c := newFakeExec(t)
		f.add(r)
		w, _ := newStripeWorker(c, WorkerOptions{})
		res, err := w.Execute(context.Background(), "req-1")
		var ie *IntentError
		if err != nil || !errors.As(res.Err, &ie) || ie.Reason != name {
			t.Errorf("%s: %+v, %v", name, res, err)
		}
	}
}

func TestResolveVariadicAndFold(t *testing.T) {
	type holder struct{ Calc calc }
	fn, err := resolveMethod(reflectValue(&holder{}), "calc.sum")
	if err != nil {
		t.Fatal(err)
	}
	in, err := decodeArgs(context.Background(), fn.Type(), []json.RawMessage{json.RawMessage("1"), json.RawMessage("2"), json.RawMessage("3")})
	if err != nil {
		t.Fatal(err)
	}
	if got := fn.Call(in)[0].Int(); got != 3 {
		t.Errorf("Sum got %d args", got)
	}
	if _, err := resolveMethod(reflectValue(&fakeStripe{V1Transfers: &transfersService{}}), "secret.len"); err == nil {
		t.Error("an unexported field was reachable")
	}
}

func TestWorkerExecute_ClaimRefused(t *testing.T) {
	f, c := newFakeExec(t)
	f.add(authorized(t, "req-1", "stripe.transfer", "stripe", "v1transfers.create", transferArgs()))
	f.conflict["req-1"] = "already_claimed"
	w, svc := newStripeWorker(c, WorkerOptions{})
	_, err := w.Execute(context.Background(), "req-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "already_claimed" {
		t.Fatalf("err = %v", err)
	}
	if len(svc.calls) != 0 || len(f.reports) != 0 {
		t.Error("ran or reported without a claim")
	}
}

func TestWorkerRun_DrainsOnCancel(t *testing.T) {
	f, c := newFakeExec(t)
	for _, id := range []string{"req-1", "req-2", "req-3"} {
		f.add(authorized(t, id, "slow.op", "slow", "go"))
	}
	w := NewWorker(c, WorkerOptions{Keys: [][]byte{testKey}, Concurrency: 2})
	release := make(chan struct{})
	var mu sync.Mutex
	running, maxRunning, done := 0, 0, 0
	w.Handle("slow.go", func(ctx context.Context, x Execution, _ json.RawMessage) (string, error) {
		mu.Lock()
		running++
		maxRunning = max(maxRunning, running)
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		done++
		mu.Unlock()
		return x.RequestID, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- w.Run(ctx, time.Millisecond) }()
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return running == 2 })
	cancel()
	select {
	case <-stopped:
		t.Fatal("Run returned with runs in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if maxRunning != 2 || done != 2 {
		t.Errorf("maxRunning %d, done %d", maxRunning, done)
	}
	for _, id := range []string{"req-1", "req-2"} {
		if rep := f.report(id); rep["status"] != "succeeded" {
			t.Errorf("%s: a drained run should finish and report, got %v", id, rep)
		}
	}
}

func TestWorkerPoll(t *testing.T) {
	f, c := newFakeExec(t)
	for _, id := range []string{"req-1", "req-2", "req-3", "req-4", "req-5"} {
		f.add(authorized(t, id, "slow.op", "slow", "go", id))
	}
	f.conflict["req-5"] = "already_claimed"
	w := NewWorker(c, WorkerOptions{Keys: [][]byte{testKey}, Concurrency: 2})
	var mu sync.Mutex
	running, maxRunning := 0, 0
	w.Handle("slow.go", func(_ context.Context, x Execution, args json.RawMessage) (string, error) {
		mu.Lock()
		running++
		maxRunning = max(maxRunning, running)
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		var in []string
		if err := json.Unmarshal(args, &in); err != nil || len(in) != 1 || in[0] != x.RequestID {
			return "", errors.New("args " + string(args))
		}
		return x.RequestID, nil
	})

	ran, err := w.Poll(context.Background())
	if err != nil || ran != 4 {
		t.Fatalf("Poll = %d, %v; want 4, nil", ran, err)
	}
	if maxRunning != 2 {
		t.Errorf("maxRunning %d, want Concurrency", maxRunning)
	}
	for _, id := range []string{"req-1", "req-2", "req-3", "req-4"} {
		if rep := f.report(id); rep["status"] != "succeeded" || rep["reference"] != id {
			t.Errorf("%s: %v", id, rep)
		}
	}
	if ran, err := w.Poll(context.Background()); err != nil || ran != 0 {
		t.Errorf("second Poll = %d, %v; want 0, nil", ran, err)
	}
}

func TestWorkerHandler(t *testing.T) {
	f, c := newFakeExec(t)
	r := authorized(t, "req-1", "stripe.transfer", "stripe", "v1transfers.create", transferArgs())
	f.add(r)
	w, svc := newStripeWorker(c, WorkerOptions{})
	cbKey := CallbackSecret("outis_sk_test")
	h := w.HandlerKeys([]byte("whsec_a"), cbKey)

	post := func(typ string, key []byte) int {
		body, _ := json.Marshal(Event{ID: "evt_1", Type: typ, CreatedAt: time.Now().UTC(), Org: "org_1", Data: EventData{Request: *r}})
		req := httptest.NewRequest(http.MethodPost, "/outis", bytes.NewReader(body))
		req.Header.Set(SignatureHeader, SignWebhookKey(body, key, time.Now()))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(EventRequestAuthorized, []byte("whsec_wrong")); code != http.StatusBadRequest {
		t.Errorf("bad signature: %d", code)
	}
	if code := post(EventRequestDenied, []byte("whsec_a")); code != http.StatusOK {
		t.Errorf("other event: %d", code)
	}
	if code := post(EventRequestAuthorized, cbKey); code != http.StatusAccepted {
		t.Fatalf("authorized: %d", code)
	}
	w.Wait()
	if len(svc.calls) != 1 || f.report("req-1")["status"] != "succeeded" {
		t.Errorf("calls %d", len(svc.calls))
	}
}

func TestCallbackSecret(t *testing.T) {
	// The server pins the same value, computed with openssl.
	const want = "640d74bb93646ec2c790001a14516d3507d38673bc2b7bfb2917287c5ea1ff2b"
	if got := hexString(CallbackSecret("outis_sk_test")); got != want {
		t.Errorf("CallbackSecret = %s", got)
	}
}

func TestAPIErrorPrefersKind(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusConflict, `{"error":"no","kind":"already_reported","code":"legacy"}`)
	})
	_, err := c.Requests.Claim(context.Background(), "req-1", 0)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "already_reported" {
		t.Fatalf("err = %v", err)
	}
	c, _ = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, `{"error":"no","code":"not_found"}`)
	})
	if _, err := c.Requests.Retrieve(context.Background(), "x"); !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
		t.Fatalf("code fallback: %v", err)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}

func reflectValue(v any) reflect.Value { return reflect.ValueOf(v) }

func hexString(b []byte) string { return hex.EncodeToString(b) }

func TestDecodeIntent_Strict(t *testing.T) {
	cases := map[string]string{
		`{"v":1,"client":"c","method":"m","args":[],"extra":1}`:        "bad_intent: unknown field extra",
		`{"v":1,"client":"c","method":"m","args":[],"kwargs":{"a":1}}`: "bad_intent: kwargs not supported",
		`{"v":1,"client":"c","method":"m","args":[],"kwargs":"x"}`:     "bad_intent: kwargs isn't an object",
	}
	for pt, want := range cases {
		_, err := DecodeIntent([]byte(pt))
		var ie *IntentError
		if !errors.As(err, &ie) || ie.Reason != ReasonBadIntent || err.Error() != want {
			t.Errorf("%s: %v, want %s", pt, err, want)
		}
	}
	for _, pt := range []string{
		`{"v":1,"client":"c","method":"m","args":[1]}`,
		`{"v":1,"client":"c","method":"m","args":[1],"kwargs":{}}`,
		`{"v":1,"client":"c","method":"m","args":[1],"kwargs":null}`,
	} {
		if _, err := DecodeIntent([]byte(pt)); err != nil {
			t.Errorf("%s refused: %v", pt, err)
		}
	}
}

func TestWorkerExecute_RefusesKwargs(t *testing.T) {
	f, c := newFakeExec(t)
	pt := []byte(`{"v":1,"client":"stripe","method":"v1transfers.create","args":[{"amount":1}],"kwargs":{"idempotency_key":"x"}}`)
	env, err := SealIntent(testKey, "stripe.transfer", pt)
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{IntentParamKey: IntentDigest(pt)}
	f.add(&Request{ID: "req-1", Action: "stripe.transfer", Outcome: OutcomeAuthorized, Params: params,
		OperationHash: OperationHash("stripe.transfer", params), Intent: env})
	w, svc := newStripeWorker(c, WorkerOptions{})
	res, err := w.Execute(context.Background(), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ExecutionFailed || len(svc.calls) != 0 {
		t.Fatalf("result %+v, calls %d", res, len(svc.calls))
	}
	if rep := f.report("req-1"); rep["error"] != "bad_intent: kwargs not supported" {
		t.Errorf("report = %v", rep)
	}
}
