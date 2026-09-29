package outis

import (
	"encoding/json"
	"time"
)

// State is where a request is in its life.
type State string

// A request is pending while proposed, notified or staging. Executing,
// succeeded and failed describe an integration's own call after
// authorization; a request made through the API goes from authorized to
// succeeded with nothing run.
const (
	StateProposed   State = "proposed"
	StateNotified   State = "notified"
	StateStaging    State = "staging"
	StateAuthorized State = "authorized"
	StateExecuting  State = "executing"
	StateSucceeded  State = "succeeded"
	StateFailed     State = "failed"
	StateExpired    State = "expired"
	StateAborted    State = "aborted"
	StateDenied     State = "denied"
)

// Outcome is how a request ended. It's empty while the request is live.
type Outcome string

// The outcomes. Denied is a rejection, aborted is a cancellation.
const (
	OutcomeAuthorized Outcome = "authorized"
	OutcomeDenied     Outcome = "denied"
	OutcomeExpired    Outcome = "expired"
	OutcomeAborted    Outcome = "aborted"
)

// Request is one request as the API reports it.
type Request struct {
	ID        string
	Action    string
	Requester string
	State     State
	// Live is true while the request can still change.
	Live bool
	// Outcome is empty while Live.
	Outcome Outcome
	// Approvers are the operators who turned a key. Empty unless authorized.
	Approvers []string
	// Params are what the operators saw.
	Params        map[string]string
	OperationHash string
	CreatedAt     time.Time
	// DecidedAt is zero until the request is decided.
	DecidedAt time.Time
	// Intent is the sealed call the request carries, or nil. Outis can't read it.
	Intent *IntentEnvelope
	// Execution is a worker's run of the intent. Its State is empty when the
	// server didn't report one.
	Execution ExecutionRecord
	// Replayed is true when Create returned an existing request for the same
	// idempotency key instead of creating one.
	Replayed bool
}

// IsPending reports whether the request is still waiting on people.
func (r *Request) IsPending() bool { return r.Live }

// IsAuthorized reports whether the required people authorized it.
func (r *Request) IsAuthorized() bool { return r.Outcome == OutcomeAuthorized }

// Operation returns the action and params the request carries.
func (r *Request) Operation() Operation { return Operation{Action: r.Action, Params: r.Params} }

// wireRequest is the API's request object: snake_case, times in epoch milliseconds.
type wireRequest struct {
	ID            string            `json:"id"`
	Action        string            `json:"action"`
	Requester     string            `json:"requester,omitempty"`
	State         State             `json:"state"`
	Live          bool              `json:"live"`
	Outcome       *Outcome          `json:"outcome"`
	Approvers     []string          `json:"approvers"`
	Params        map[string]string `json:"params"`
	OperationHash string            `json:"operation_hash,omitempty"`
	CreatedAt     *int64            `json:"created_at"`
	DecidedAt     *int64            `json:"decided_at"`
	Intent        *IntentEnvelope   `json:"intent,omitempty"`
	Execution     *wireExecution    `json:"execution,omitempty"`
}

// ExecutionState is where a worker's run of an intent stands.
type ExecutionState string

// The execution states. None means the request carries no intent; pending
// means it's authorized and nobody has claimed it.
const (
	ExecutionNone      ExecutionState = "none"
	ExecutionPending   ExecutionState = "pending"
	ExecutionClaimed   ExecutionState = "claimed"
	ExecutionSucceeded ExecutionState = "succeeded"
	ExecutionFailed    ExecutionState = "failed"
)

// ExecutionRecord is Outis's record of a worker's run of an intent. Zero
// times are unset.
type ExecutionRecord struct {
	State          ExecutionState
	ClaimedAt      time.Time
	LeaseExpiresAt time.Time
	ReportedAt     time.Time
	// ExecuteBy is when the request stops being claimable.
	ExecuteBy time.Time
	// Reference is what the worker reported on success, such as a transfer id.
	Reference string
	// Error is what the worker reported on failure.
	Error string
}

type wireExecution struct {
	State          ExecutionState `json:"state"`
	ClaimedAt      *int64         `json:"claimed_at"`
	LeaseExpiresAt *int64         `json:"lease_expires_at"`
	ReportedAt     *int64         `json:"reported_at"`
	Reference      *string        `json:"reference"`
	Error          *string        `json:"error"`
	ExecuteBy      *int64         `json:"execute_by"`
}

// UnmarshalJSON reads the API's wire form.
func (r *Request) UnmarshalJSON(b []byte) error {
	var w wireRequest
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*r = Request{
		ID:            w.ID,
		Action:        w.Action,
		Requester:     w.Requester,
		State:         w.State,
		Live:          w.Live,
		Approvers:     w.Approvers,
		Params:        w.Params,
		OperationHash: w.OperationHash,
		CreatedAt:     fromMillis(w.CreatedAt),
		DecidedAt:     fromMillis(w.DecidedAt),
		Intent:        w.Intent,
	}
	if x := w.Execution; x != nil {
		r.Execution = ExecutionRecord{
			State:          x.State,
			ClaimedAt:      fromMillis(x.ClaimedAt),
			LeaseExpiresAt: fromMillis(x.LeaseExpiresAt),
			ReportedAt:     fromMillis(x.ReportedAt),
			ExecuteBy:      fromMillis(x.ExecuteBy),
			Reference:      deref(x.Reference),
			Error:          deref(x.Error),
		}
	}
	if w.Outcome != nil {
		r.Outcome = *w.Outcome
	}
	return nil
}

// MarshalJSON writes the API's wire form, so a stored Request reads back unchanged.
func (r Request) MarshalJSON() ([]byte, error) {
	w := wireRequest{
		ID:            r.ID,
		Action:        r.Action,
		Requester:     r.Requester,
		State:         r.State,
		Live:          r.Live,
		Approvers:     r.Approvers,
		Params:        r.Params,
		OperationHash: r.OperationHash,
		CreatedAt:     toMillis(r.CreatedAt),
		DecidedAt:     toMillis(r.DecidedAt),
		Intent:        r.Intent,
	}
	if x := r.Execution; x.State != "" {
		w.Execution = &wireExecution{
			State:          x.State,
			ClaimedAt:      toMillis(x.ClaimedAt),
			LeaseExpiresAt: toMillis(x.LeaseExpiresAt),
			ReportedAt:     toMillis(x.ReportedAt),
			ExecuteBy:      toMillis(x.ExecuteBy),
			Reference:      ref(x.Reference),
			Error:          ref(x.Error),
		}
	}
	if w.Approvers == nil {
		w.Approvers = []string{}
	}
	if w.Params == nil {
		w.Params = map[string]string{}
	}
	if r.Outcome != "" {
		o := r.Outcome
		w.Outcome = &o
	}
	return json.Marshal(w)
}

func fromMillis(ms *int64) time.Time {
	if ms == nil {
		return time.Time{}
	}
	return time.UnixMilli(*ms).UTC()
}

func toMillis(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	ms := t.UnixMilli()
	return &ms
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ref(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
