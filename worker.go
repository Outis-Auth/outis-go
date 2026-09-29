package outis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"sync"
	"time"
)

// WorkerOptions configures a [Worker]. Keys is required.
type WorkerOptions struct {
	// Keys are the intent keys the worker may decrypt with, matched by kid,
	// so an old key can stay while intents sealed under it drain.
	Keys [][]byte
	// Allow, when set, limits what runs to "client.method" names matching one
	// of these path.Match patterns, like "stripe.transfers.*".
	Allow []string
	// Concurrency bounds how many intents Run, Poll and the webhook handler
	// run at once. Zero means 4.
	Concurrency int
	// Lease is how long each claim holds. Zero means [DefaultLease]. Make it
	// longer than your slowest call.
	Lease time.Duration
	// OnResult, when set, sees every finished execution.
	OnResult func(Result)
	// Logger gets poll and report errors. Nil means slog.Default().
	Logger *slog.Logger
}

// Execution is what a [HandlerFunc] gets about the run it's making.
type Execution struct {
	RequestID string
	ClaimID   string
	Request   *Request
	Client    string
	Method    string
	// IdempotencyKey is the request id. Pass it to the downstream call so a
	// run retried after a lost lease can't happen twice.
	IdempotencyKey string
	LeaseExpiresAt time.Time
}

// HandlerFunc runs an intent that reflection can't reach. args is the
// intent's JSON array of arguments. The returned reference is reported on
// success, such as the id of what was created.
type HandlerFunc func(ctx context.Context, x Execution, args json.RawMessage) (reference string, err error)

// Result is how one execution went.
type Result struct {
	RequestID string
	ClaimID   string
	Client    string
	Method    string
	// Status is ExecutionSucceeded or ExecutionFailed.
	Status    ExecutionState
	Reference string
	// Err is why it failed, an [*IntentError] whose Reason says which check
	// or step refused.
	Err error
}

// Worker runs authorized intents with the clients registered on it. It
// claims each request before running it, verifies the sealed intent against
// what the operators approved, and reports the outcome to Outis.
type Worker struct {
	c    *Client
	opts WorkerOptions
	log  *slog.Logger
	sem  chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	clients  map[string]*registered
	handlers map[string]HandlerFunc
	inflight map[string]bool
}

// NewWorker returns a worker using c, whose key needs the read and execute
// scopes.
func NewWorker(c *Client, opts WorkerOptions) *Worker {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.Lease == 0 {
		opts.Lease = DefaultLease
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		c:        c,
		opts:     opts,
		log:      log,
		sem:      make(chan struct{}, opts.Concurrency),
		clients:  map[string]*registered{},
		handlers: map[string]HandlerFunc{},
		inflight: map[string]bool{},
	}
}

// RegisterOption adjusts one [Worker.Register].
type RegisterOption func(*registered)

// InjectIdempotencyKey opts a client in to having the request id set on each
// call's arguments: any argument with a SetIdempotencyKey(string) method,
// like stripe-go's params, gets it before the call.
func InjectIdempotencyKey() RegisterOption {
	return func(r *registered) { r.injectKey = true }
}

type registered struct {
	target    any
	injectKey bool
}

// Register makes client callable by intents naming it. An intent's method is
// a dotted path over exported fields, accessor methods and finally a method
// (matched ignoring case and underscores, so "transfers.create" finds
// Transfers.Create). JSON args decode into the method's parameter types, a
// leading context.Context is supplied, and a trailing error is honoured. The
// result's ID field or GetID() is reported as the reference. Register panics
// on an empty name, a nil client or a name used twice.
func (w *Worker) Register(name string, client any, opts ...RegisterOption) {
	if name == "" || client == nil {
		panic("outis: Register needs a name and a client")
	}
	r := &registered{target: client}
	for _, o := range opts {
		o(r)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, dup := w.clients[name]; dup {
		panic("outis: client " + name + " registered twice")
	}
	w.clients[name] = r
}

// Handle makes fn run intents whose "client.method" is name, like
// "db.restore". A handler wins over a registered client with the same name.
// Handle panics on an empty name, a nil fn or a name used twice.
func (w *Worker) Handle(name string, fn HandlerFunc) {
	if name == "" || fn == nil {
		panic("outis: Handle needs a name and a func")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, dup := w.handlers[name]; dup {
		panic("outis: handler " + name + " registered twice")
	}
	w.handlers[name] = fn
}

type idempotencyKeyCtx struct{}

// IdempotencyKey is the key for the run ctx belongs to, or "" outside one.
// Handlers and registered methods that take a context can pass it downstream.
func IdempotencyKey(ctx context.Context) string {
	k, _ := ctx.Value(idempotencyKeyCtx{}).(string)
	return k
}

// Execute claims request id, verifies its intent, runs it and reports the
// outcome. The error is only for what kept it from a report (the claim was
// refused, or the report didn't land); a run that failed or was refused is
// reported, and its Result carries the reason.
func (w *Worker) Execute(ctx context.Context, id string) (*Result, error) {
	claim, err := w.c.Requests.Claim(ctx, id, w.opts.Lease)
	if err != nil {
		return nil, err
	}
	r := claim.Request
	res := &Result{RequestID: id, ClaimID: claim.ID}
	x := Execution{
		RequestID:      id,
		ClaimID:        claim.ID,
		Request:        r,
		IdempotencyKey: id,
		LeaseExpiresAt: claim.LeaseExpiresAt,
	}

	ref, runErr := w.verifyAndRun(ctx, &x)
	res.Client, res.Method = x.Client, x.Method
	rep := ExecutionReport{ClaimID: claim.ID}
	if runErr != nil {
		res.Status, res.Err = ExecutionFailed, runErr
		rep.Status, rep.Error = ExecutionFailed, runErr.Error()
	} else {
		res.Status, res.Reference = ExecutionSucceeded, ref
		rep.Status, rep.Reference = ExecutionSucceeded, ref
	}

	// The run happened, so the report goes out even if ctx ended meanwhile.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := w.c.Requests.ReportExecution(rctx, id, rep); err != nil {
		return res, fmt.Errorf("outis: request %s ran (%s) but the report failed: %w", id, res.Status, err)
	}
	if w.opts.OnResult != nil {
		w.opts.OnResult(*res)
	}
	return res, nil
}

// verifyAndRun makes every check the spec lists, in order, then the call.
func (w *Worker) verifyAndRun(ctx context.Context, x *Execution) (string, error) {
	r := x.Request
	pt, err := OpenIntent(w.opts.Keys, r.Action, r.Intent)
	if err != nil {
		return "", err
	}
	if d := IntentDigest(pt); r.Params[IntentParamKey] != d {
		return "", &IntentError{Reason: ReasonDigestMismatch, Err: fmt.Errorf("params.intent isn't %s", d)}
	}
	if want := OperationHash(r.Action, r.Params); r.OperationHash != want {
		return "", &IntentError{Reason: ReasonOperationMismatch, Err: fmt.Errorf("operation_hash isn't %s", want)}
	}
	if !r.IsAuthorized() {
		return "", &IntentError{Reason: ReasonNotAuthorized, Err: fmt.Errorf("outcome is %q", r.Outcome)}
	}
	in, err := DecodeIntent(pt)
	if err != nil {
		return "", err
	}
	x.Client, x.Method = in.Client, in.Method
	name := in.Client + "." + in.Method

	w.mu.Lock()
	h := w.handlers[name]
	reg := w.clients[in.Client]
	w.mu.Unlock()
	if h == nil && reg == nil {
		return "", &IntentError{Reason: ReasonNotRegistered, Err: fmt.Errorf("nothing registered for %s", name)}
	}
	if !w.allowed(name) {
		return "", &IntentError{Reason: ReasonNotAllowed, Err: fmt.Errorf("%s isn't in Allow", name)}
	}

	ctx = context.WithValue(ctx, idempotencyKeyCtx{}, x.IdempotencyKey)
	if h != nil {
		args, _ := json.Marshal(in.Args)
		return callHandler(ctx, h, *x, args)
	}
	return callMethod(ctx, reg, in.Method, in.Args, x.IdempotencyKey)
}

func (w *Worker) allowed(name string) bool {
	if len(w.opts.Allow) == 0 {
		return true
	}
	for _, p := range w.opts.Allow {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func callHandler(ctx context.Context, h HandlerFunc, x Execution, args json.RawMessage) (ref string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &IntentError{Reason: ReasonExecutionError, Err: fmt.Errorf("panic: %v", p)}
		}
	}()
	ref, err = h(ctx, x, args)
	if err != nil {
		return "", &IntentError{Reason: ReasonExecutionError, Err: err}
	}
	return ref, nil
}

// Run polls Outis every interval for claimable requests and runs them, at
// most Concurrency at a time. When ctx ends it stops polling, lets the runs
// in flight finish, and returns nil.
func (w *Worker) Run(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		return errors.New("outis: Run needs an interval greater than zero")
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		w.poll(ctx)
		select {
		case <-ctx.Done():
			w.wg.Wait()
			return nil
		case <-t.C:
		}
	}
}

func (w *Worker) poll(ctx context.Context) {
	free := cap(w.sem) - len(w.sem)
	if free == 0 || ctx.Err() != nil {
		return
	}
	reqs, err := w.c.Requests.ListExecutable(ctx, min(free, 100))
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("outis: list executable requests", "err", err)
		}
		return
	}
	for _, r := range reqs {
		w.dispatch(ctx, r.ID)
	}
}

// Poll lists the executable requests once, runs them at most Concurrency at
// a time, and returns how many it ran once they've all finished. It's for
// cron jobs and serverless hosts that can't keep [Worker.Run] going. The
// error joins a failed list, runs whose report didn't land, and ctx ending
// before every request started.
func (w *Worker) Poll(ctx context.Context) (int, error) {
	reqs, err := w.c.Requests.ListExecutable(ctx, min(w.opts.Concurrency*4, 100))
	if err != nil {
		return 0, err
	}
	var (
		mu   sync.Mutex
		ran  int
		errs []error
		wg   sync.WaitGroup
	)
	for _, r := range reqs {
		select {
		case w.sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ran, errors.Join(append(errs, ctx.Err())...)
		}
		w.mu.Lock()
		if w.inflight[r.ID] {
			w.mu.Unlock()
			<-w.sem
			continue
		}
		w.inflight[r.ID] = true
		w.wg.Add(1)
		w.mu.Unlock()
		wg.Add(1)
		w.spawn(ctx, r.ID, func(res *Result, err error) {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			if res != nil {
				ran++
			}
			if err != nil && !notClaimable(err) {
				errs = append(errs, err)
			}
		})
	}
	wg.Wait()
	return ran, errors.Join(errs...)
}

type dispatched int

const (
	started dispatched = iota
	running
	busy
)

// dispatch starts id in the background if a slot is free and it isn't
// already running here. The run outlives ctx's cancellation; Run and Wait
// drain it.
func (w *Worker) dispatch(ctx context.Context, id string) dispatched {
	w.mu.Lock()
	if w.inflight[id] {
		w.mu.Unlock()
		return running
	}
	select {
	case w.sem <- struct{}{}:
	default:
		w.mu.Unlock()
		return busy
	}
	w.inflight[id] = true
	w.wg.Add(1)
	w.mu.Unlock()

	w.spawn(ctx, id, func(_ *Result, err error) {
		switch {
		case err == nil:
		case notClaimable(err):
			w.log.Debug("outis: request not claimable", "request", id, "err", err)
		default:
			w.log.Error("outis: execute", "request", id, "err", err)
		}
	})
	return started
}

// spawn runs id in the background on a slot the caller already holds, with
// id marked in flight and counted in w.wg, and hands the outcome to done.
func (w *Worker) spawn(ctx context.Context, id string, done func(*Result, error)) {
	go func() {
		res, err := w.Execute(context.WithoutCancel(ctx), id)
		w.mu.Lock()
		delete(w.inflight, id)
		w.mu.Unlock()
		<-w.sem
		w.wg.Done()
		done(res, err)
	}()
}

// notClaimable reports whether err is a claim refused because someone else
// holds the request or it's already done.
func notClaimable(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusConflict || apiErr.StatusCode == http.StatusGone)
}

// Wait blocks until every run that Run, Poll or a webhook handler started has
// finished. Call it after shutting down the HTTP server a handler is on.
func (w *Worker) Wait() { w.wg.Wait() }

// Handler receives Outis webhooks signed with an endpoint secret (whsec_...)
// and starts each authorized request that carries an intent. It answers 202
// once the run starts, and 503 when every slot is busy so Outis delivers
// again later.
func (w *Worker) Handler(secret string) http.Handler {
	return w.HandlerKeys([]byte(secret))
}

// HandlerKeys is [Worker.Handler] with raw HMAC keys, any of which may
// match, such as an endpoint secret and [CallbackSecret] together.
func (w *Worker) HandlerKeys(keys ...[]byte) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			rw.Header().Set("Allow", http.MethodPost)
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, 1<<20))
		if err != nil {
			http.Error(rw, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		ev, err := VerifyWebhookKeys(body, r.Header, keys, nil)
		if err != nil {
			http.Error(rw, "bad signature", http.StatusBadRequest)
			return
		}
		req := ev.Data.Request
		if ev.Type != EventRequestAuthorized || req.Intent == nil || req.ID == "" {
			rw.WriteHeader(http.StatusOK)
			return
		}
		switch w.dispatch(context.WithoutCancel(r.Context()), req.ID) {
		case busy:
			rw.Header().Set("Retry-After", "30")
			http.Error(rw, "worker busy", http.StatusServiceUnavailable)
		default:
			rw.WriteHeader(http.StatusAccepted)
		}
	})
}
