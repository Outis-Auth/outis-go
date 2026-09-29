package outis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// MaxLease caps a claim's lease.
const MaxLease = time.Hour

// DefaultLease is a claim's lease when none is given.
const DefaultLease = 10 * time.Minute

// maxReportLen is the server's cap on a report's reference and error.
const maxReportLen = 512

// Claim is a worker's exclusive hold on running one request's intent, until
// LeaseExpiresAt. After that another worker may claim it.
type Claim struct {
	ID             string
	LeaseExpiresAt time.Time
	Request        *Request
}

// ExecutionReport is how a worker's run went. Status is [ExecutionSucceeded]
// or [ExecutionFailed]; Reference and Error are cut to 512 characters.
type ExecutionReport struct {
	ClaimID   string
	Status    ExecutionState
	Reference string
	Error     string
}

// ListExecutable returns the org's requests a worker may claim now:
// authorized, carrying an intent, unreported, unleased and inside their
// execution window, oldest first. limit is 1 to 100. The key needs the
// execute scope.
func (s *RequestsService) ListExecutable(ctx context.Context, limit int) ([]*Request, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("outis: list executable: limit is 1 to 100")
	}
	q := url.Values{"executable": {"true"}, "limit": {strconv.Itoa(limit)}}
	var out struct {
		Requests []*Request `json:"requests"`
	}
	if _, err := s.c.call(ctx, http.MethodGet, "/v1/requests?"+q.Encode(), nil, nil, true, &out); err != nil {
		return nil, err
	}
	return out.Requests, nil
}

// Claim takes the lease on running a request's intent. Outis answers 409
// already_claimed, already_reported or not_authorized, or 410
// execution_window_closed, as an [*APIError]. A claim isn't retried, since a
// lost response would leave the lease held by nobody who knows its id.
func (s *RequestsService) Claim(ctx context.Context, id string, lease time.Duration) (*Claim, error) {
	if id == "" {
		return nil, errors.New("outis: claim: id is required")
	}
	if lease == 0 {
		lease = DefaultLease
	}
	if lease < time.Second || lease > MaxLease {
		return nil, fmt.Errorf("outis: claim: a lease is 1s to %s", MaxLease)
	}
	body, _ := json.Marshal(map[string]int64{"lease_seconds": int64(lease / time.Second)})
	var out struct {
		ClaimID        string  `json:"claim_id"`
		LeaseExpiresAt *int64  `json:"lease_expires_at"`
		Request        Request `json:"request"`
	}
	if _, err := s.c.call(ctx, http.MethodPost, "/v1/requests/"+url.PathEscape(id)+"/claim", body, nil, false, &out); err != nil {
		return nil, err
	}
	if out.ClaimID == "" {
		return nil, errors.New("outis: claim: the response carried no claim id")
	}
	return &Claim{ID: out.ClaimID, LeaseExpiresAt: fromMillis(out.LeaseExpiresAt), Request: &out.Request}, nil
}

// ReportExecution records how a claimed run went. Only the claim's holder
// may report, and repeating the same report is harmless, so it's retried.
func (s *RequestsService) ReportExecution(ctx context.Context, id string, rep ExecutionReport) error {
	if id == "" || rep.ClaimID == "" {
		return errors.New("outis: report: id and claim id are required")
	}
	if rep.Status != ExecutionSucceeded && rep.Status != ExecutionFailed {
		return fmt.Errorf("outis: report: status is succeeded or failed, not %q", rep.Status)
	}
	body, err := json.Marshal(struct {
		ClaimID   string         `json:"claim_id"`
		Status    ExecutionState `json:"status"`
		Reference string         `json:"reference,omitempty"`
		Error     string         `json:"error,omitempty"`
	}{rep.ClaimID, rep.Status, truncate(rep.Reference), truncate(rep.Error)})
	if err != nil {
		return fmt.Errorf("outis: report: %w", err)
	}
	_, err = s.c.call(ctx, http.MethodPost, "/v1/requests/"+url.PathEscape(id)+"/execution", body, nil, true, nil)
	return err
}

// truncate cuts s to maxReportLen characters without splitting a rune.
func truncate(s string) string {
	n := 0
	for i := range s {
		if n == maxReportLen {
			return s[:i]
		}
		n++
	}
	return s
}
