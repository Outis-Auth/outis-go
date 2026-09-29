package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	outis "github.com/outis-auth/outis-go"
)

// fakeOutis answers every create with a live request and every read with
// outcome, or with a live request when outcome is empty.
func fakeOutis(t *testing.T, outcome outis.Outcome) (env, *[]string) {
	t.Helper()
	var calls []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		req := outis.Request{
			ID: "req-1", Action: "db.restore", Requester: "keith", State: outis.StateNotified, Live: true,
			Params: map[string]string{"db": "payments"}, CreatedAt: time.UnixMilli(1788350100000),
		}
		req.OperationHash = req.Operation().Hash()
		status := http.StatusAccepted
		if r.Method == http.MethodGet {
			status = http.StatusOK
			if outcome != "" {
				req.Live, req.Outcome, req.State = false, outcome, outis.State(outcome)
				req.DecidedAt = time.UnixMilli(1788350400000)
				if outcome == outis.OutcomeAuthorized {
					req.Approvers = []string{"maya", "sam"}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"server_now": 1788350400000, "request": req})
	}))
	t.Cleanup(srv.Close)
	vars := map[string]string{"OUTIS_API_KEY": "outis_sk_test", "OUTIS_BASE_URL": srv.URL}
	return func(k string) string { return vars[k] }, &calls
}

func runCLI(getenv env, stdin string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, getenv, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func needShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("gate's exec tests drive /bin/sh")
	}
}

func gateArgs(timeout string, command ...string) []string {
	return append([]string{"gate", "-action", "db.restore", "-requester", "keith", "-param", "db=payments", "-timeout", timeout}, command...)
}

func TestGate_AuthorizedRunsCommand(t *testing.T) {
	needShell(t)
	getenv, _ := fakeOutis(t, outis.OutcomeAuthorized)
	marker := filepath.Join(t.TempDir(), "ran")

	code, out, stderr := runCLI(getenv, "", gateArgs("5m", "sh", "-c", `printf '%s %s' "$OUTIS_REQUEST_ID" "$1" > "$0"; echo restored`, marker, "arg1")...)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	if out != "restored\n" {
		t.Errorf("command stdout not passed through: %q", out)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "req-1 arg1" {
		t.Errorf("command saw %q (%v), want the request id and its args", got, err)
	}
}

func TestGate_PassesThroughExitCode(t *testing.T) {
	needShell(t)
	getenv, _ := fakeOutis(t, outis.OutcomeAuthorized)
	if code, _, _ := runCLI(getenv, "", gateArgs("5m", "sh", "-c", "exit 42")...); code != 42 {
		t.Errorf("exit %d, want the command's 42", code)
	}
}

func TestGate_NotAuthorizedDoesNotRun(t *testing.T) {
	needShell(t)
	for _, outcome := range []outis.Outcome{outis.OutcomeDenied, outis.OutcomeExpired, outis.OutcomeAborted} {
		t.Run(string(outcome), func(t *testing.T) {
			getenv, _ := fakeOutis(t, outcome)
			marker := filepath.Join(t.TempDir(), "ran")
			code, _, stderr := runCLI(getenv, "", gateArgs("5m", "touch", marker)...)
			if code != exitNotAuthorized {
				t.Errorf("exit %d, want %d", code, exitNotAuthorized)
			}
			if !strings.Contains(stderr, string(outcome)) {
				t.Errorf("stderr doesn't name the outcome: %q", stderr)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("the command ran without authorization")
			}
		})
	}
}

func TestGate_TimeoutDoesNotRun(t *testing.T) {
	needShell(t)
	getenv, _ := fakeOutis(t, "")
	marker := filepath.Join(t.TempDir(), "ran")
	code, _, stderr := runCLI(getenv, "", gateArgs("50ms", "touch", marker)...)
	if code != exitTimeout {
		t.Errorf("exit %d, want %d (%s)", code, exitTimeout, stderr)
	}
	if !strings.Contains(stderr, "outis wait req-1") {
		t.Errorf("stderr doesn't say how to resume: %q", stderr)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran without authorization")
	}
}

func TestGate_Usage(t *testing.T) {
	getenv, calls := fakeOutis(t, outis.OutcomeAuthorized)
	cases := map[string][]string{
		"no command":   gateArgs("5m"),
		"no timeout":   {"gate", "-action", "a", "-requester", "k", "true"},
		"over the cap": gateArgs("2h", "true"),
		"no action":    {"gate", "-requester", "k", "-timeout", "5m", "true"},
	}
	for name, args := range cases {
		if code, _, _ := runCLI(getenv, "", args...); code != exitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, exitUsage)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("a usage error reached the API: %v", *calls)
	}
}

func TestGate_CommandNotFound(t *testing.T) {
	getenv, _ := fakeOutis(t, outis.OutcomeAuthorized)
	if code, _, _ := runCLI(getenv, "", gateArgs("5m", "outis-no-such-command-anywhere")...); code != exitNotFound {
		t.Errorf("exit %d, want %d", code, exitNotFound)
	}
}

func TestGate_HelpSaysItIsNotABoundary(t *testing.T) {
	code, _, stderr := runCLI(func(string) string { return "" }, "", "gate", "-h")
	if code != exitOK {
		t.Errorf("exit %d", code)
	}
	for _, want := range []string{"not a security boundary", "CI job", "restricted runner", "Kubernetes job", "privileged execution service"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("gate -h doesn't say %q", want)
		}
	}
}

func TestRequestGetWait(t *testing.T) {
	getenv, calls := fakeOutis(t, outis.OutcomeAuthorized)

	code, out, stderr := runCLI(getenv, "", "request", "-action", "db.restore", "-requester", "keith", "-param", "db=payments", "-json")
	if code != 0 {
		t.Fatalf("request: exit %d, %s", code, stderr)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil || body["id"] != "req-1" || body["outcome"] != nil {
		t.Errorf("request -json = %s (%v)", out, err)
	}

	if code, out, _ = runCLI(getenv, "", "get", "req-1"); code != 0 || !strings.HasPrefix(out, "req-1\tauthorized\tauthorized\t") {
		t.Errorf("get: exit %d, %q", code, out)
	}
	if code, _, _ = runCLI(getenv, "", "wait", "req-1", "-timeout", "5m", "-json"); code != 0 {
		t.Errorf("wait: exit %d", code)
	}
	if code, _, _ = runCLI(getenv, "", "wait", "req-1"); code != exitUsage {
		t.Errorf("wait without -timeout: exit %d", code)
	}
	want := []string{"POST /v1/requests", "GET /v1/requests/req-1", "GET /v1/requests/req-1"}
	if strings.Join(*calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v", *calls)
	}

	getenv, _ = fakeOutis(t, outis.OutcomeDenied)
	if code, _, _ = runCLI(getenv, "", "wait", "req-1", "-timeout", "5m"); code != exitNotAuthorized {
		t.Errorf("wait on a denied request: exit %d", code)
	}
}

func TestMissingAPIKey(t *testing.T) {
	if code, _, stderr := runCLI(func(string) string { return "" }, "", "get", "req-1"); code != exitUsage || !strings.Contains(stderr, "OUTIS_API_KEY") {
		t.Errorf("exit %d, %q", code, stderr)
	}
}

func TestHash(t *testing.T) {
	code, out, _ := runCLI(nil, "", "hash", "-action", "deploy.production", "-param", "repo=acme/payments-api", "-param", "env=production", "-param", "sha=8d93f71")
	if code != 0 || out != "sha256:54feb247e0ae56c01d430beb1b1c4c604384ca91832a1ccb435fe2753fb2be9e\n" {
		t.Errorf("exit %d, %q", code, out)
	}
}

func TestVerifyWebhook(t *testing.T) {
	body := `{"id":"evt_1","type":"request.denied","created_at":"2026-09-27T12:00:00Z","org":"org_1","data":{"request":{"id":"req-1","action":"a","state":"denied","live":false,"outcome":"denied","approvers":[],"params":{},"created_at":1,"decided_at":2}}}`
	vars := map[string]string{"OUTIS_WEBHOOK_SECRET": "whsec_a"}
	getenv := func(k string) string { return vars[k] }
	sig := outis.SignWebhook([]byte(body), "whsec_a", time.Now())

	code, out, stderr := runCLI(getenv, body, "verify-webhook", "-header", "Outis-Signature: "+sig)
	if code != 0 || !strings.Contains(out, `"evt_1"`) {
		t.Errorf("good delivery: exit %d, %q %q", code, out, stderr)
	}
	if code, _, _ = runCLI(getenv, body+" ", "verify-webhook", "-header", "Outis-Signature: "+sig); code != exitError {
		t.Errorf("tampered delivery: exit %d", code)
	}
	if code, _, _ = runCLI(func(string) string { return "" }, body, "verify-webhook", "-header", "Outis-Signature: "+sig); code != exitUsage {
		t.Errorf("no secret: exit %d", code)
	}
}
