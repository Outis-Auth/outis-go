package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	outis "github.com/outis-auth/outis-go"
)

// Exit codes. gate passes its command's own code through instead.
const (
	exitOK            = 0
	exitError         = 1
	exitUsage         = 2
	exitNotAuthorized = 3
	exitTimeout       = 4
	exitCannotExec    = 126
	exitNotFound      = 127
)

const usage = `outis talks to Outis, the approval gate.

Usage:
  outis request -action A -requester R [-param k=v ...] [-summary S] [-quorum N]
                [-idempotency-key K] [-callback-url U] [-json]
  outis get <id> [-json]
  outis wait <id> -timeout 5m [-json]
  outis gate -action A -requester R [-param k=v ...] -timeout 5m command [args...]
  outis verify-webhook [-secret-env NAME] -header "Outis-Signature: ..." < body
  outis hash -action A [-param k=v ...]
  outis init worker [-runtime go|temporal-go] [-dir .]

Environment:
  OUTIS_API_KEY   the API key (request, get, wait, gate)
  OUTIS_BASE_URL  the API, default https://api.outis.tech

Exit codes:
  0  done (for wait and gate: authorized)
  1  an error talking to Outis
  2  bad usage
  3  not authorized: denied, expired or aborted
  4  timed out while still pending
  gate exits with its command's own code once the command runs.

Run "outis <command> -h" for a command's flags.
`

const gateHelp = `outis gate asks Outis for a request, waits up to -timeout for it, and runs
command only if it was authorized. The command's exit code becomes gate's.
On a denial, expiry or abort gate exits 3, and when the wait runs out it exits
4. Either way the command doesn't run. The command gets OUTIS_REQUEST_ID and
OUTIS_OPERATION_HASH in its environment.

On its own, gate's not a security boundary. It only enforces anything when
the command can't be run any other way, ie the privileged credentials exist
only where gate runs:

  - a CI job whose secrets are scoped to the step that calls gate
  - a restricted runner nobody can shell into
  - a Kubernetes job whose service account only that job gets
  - a privileged execution service that only runs commands through gate

If an operator can run the command directly, gate hasn't stopped anything.

Usage:
  outis gate -action A -requester R [-param k=v ...] -timeout 5m command [args...]

Flags:
`

type env func(string) string

func run(ctx context.Context, args []string, getenv env, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "request":
		return cmdRequest(ctx, rest, getenv, stdout, stderr)
	case "get":
		return cmdGet(ctx, rest, getenv, stdout, stderr)
	case "wait":
		return cmdWait(ctx, rest, getenv, stdout, stderr)
	case "gate":
		return cmdGate(ctx, rest, getenv, stdin, stdout, stderr)
	case "verify-webhook":
		return cmdVerify(rest, getenv, stdin, stdout, stderr)
	case "hash":
		return cmdHash(rest, stdout, stderr)
	case "init":
		return cmdInit(rest, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "outis: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
}

// paramFlag collects repeated -param k=v flags.
type paramFlag map[string]string

func (p paramFlag) String() string { return "" }

func (p paramFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, got %q", s)
	}
	p[k] = v
	return nil
}

// headerFlag collects repeated -header "Name: value" flags.
type headerFlag http.Header

func (h headerFlag) String() string { return "" }

func (h headerFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, ":")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("want \"Name: value\", got %q", s)
	}
	http.Header(h).Add(strings.TrimSpace(k), strings.TrimSpace(v))
	return nil
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("outis "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse runs fs over args and reports the exit code to use when it fails.
func parse(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitUsage, false
	}
	return 0, true
}

// parseWithID reads "<id> [flags]" as well as "[flags] <id>".
func parseWithID(fs *flag.FlagSet, args []string, stderr io.Writer) (string, int, bool) {
	if code, ok := parse(fs, args); !ok {
		return "", code, false
	}
	if fs.NArg() == 0 {
		fmt.Fprintf(stderr, "%s: a request id is required\n", fs.Name())
		return "", exitUsage, false
	}
	id := fs.Arg(0)
	if code, ok := parse(fs, fs.Args()[1:]); !ok {
		return "", code, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return "", exitUsage, false
	}
	return id, 0, true
}

func client(getenv env, stderr io.Writer) (*outis.Client, bool) {
	key := getenv("OUTIS_API_KEY")
	if key == "" {
		fmt.Fprintln(stderr, "outis: OUTIS_API_KEY isn't set")
		return nil, false
	}
	var opts []outis.Option
	if u := getenv("OUTIS_BASE_URL"); u != "" {
		opts = append(opts, outis.WithBaseURL(u))
	}
	return outis.New(key, opts...), true
}

func cmdRequest(ctx context.Context, args []string, getenv env, stdout, stderr io.Writer) int {
	fs := newFlags("request", stderr)
	params := paramFlag{}
	action := fs.String("action", "", "the action to authorize (required)")
	requester := fs.String("requester", "", "who is asking (required)")
	fs.Var(params, "param", "a param as key=value, repeatable")
	summary := fs.String("summary", "", "one line for the operators")
	quorum := fs.Int("quorum", 0, "the key count to ask for, if policy allows choosing")
	idem := fs.String("idempotency-key", "", "sent as Idempotency-Key")
	callback := fs.String("callback-url", "", "posted the signed decision")
	asJSON := fs.Bool("json", false, "print the request as JSON")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *action == "" || *requester == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "outis request: -action and -requester are required, and nothing else")
		return exitUsage
	}
	c, ok := client(getenv, stderr)
	if !ok {
		return exitUsage
	}
	var opts []outis.CreateOption
	if *idem != "" {
		opts = append(opts, outis.WithIdempotencyKey(*idem))
	}
	r, err := c.Requests.Create(ctx, outis.CreateParams{
		Action:      *action,
		Requester:   *requester,
		Params:      params,
		Summary:     *summary,
		CallbackURL: *callback,
		Quorum:      *quorum,
	}, opts...)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	return printRequest(stdout, stderr, r, *asJSON)
}

func cmdGet(ctx context.Context, args []string, getenv env, stdout, stderr io.Writer) int {
	fs := newFlags("get", stderr)
	asJSON := fs.Bool("json", false, "print the request as JSON")
	id, code, ok := parseWithID(fs, args, stderr)
	if !ok {
		return code
	}
	c, ok := client(getenv, stderr)
	if !ok {
		return exitUsage
	}
	r, err := c.Requests.Retrieve(ctx, id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	return printRequest(stdout, stderr, r, *asJSON)
}

func cmdWait(ctx context.Context, args []string, getenv env, stdout, stderr io.Writer) int {
	fs := newFlags("wait", stderr)
	timeout := fs.Duration("timeout", 0, "how long to wait, at most 30m (required)")
	asJSON := fs.Bool("json", false, "print the request as JSON")
	id, code, ok := parseWithID(fs, args, stderr)
	if !ok {
		return code
	}
	c, ok := client(getenv, stderr)
	if !ok {
		return exitUsage
	}
	r, err := c.Requests.WaitFor(ctx, id, *timeout)
	if code, done := waitFailed(err, stderr); done {
		return code
	}
	if c := printRequest(stdout, stderr, r, *asJSON); c != exitOK {
		return c
	}
	if !r.IsAuthorized() {
		return exitNotAuthorized
	}
	return exitOK
}

func cmdGate(ctx context.Context, args []string, getenv env, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("gate", stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), gateHelp)
		fs.PrintDefaults()
	}
	params := paramFlag{}
	action := fs.String("action", "", "the action to authorize (required)")
	requester := fs.String("requester", "", "who is asking (required)")
	fs.Var(params, "param", "a param as key=value, repeatable")
	summary := fs.String("summary", "", "one line for the operators")
	quorum := fs.Int("quorum", 0, "the key count to ask for, if policy allows choosing")
	idem := fs.String("idempotency-key", "", "sent as Idempotency-Key")
	timeout := fs.Duration("timeout", 0, "how long to wait, at most 30m (required)")
	asJSON := fs.Bool("json", false, "print the decided request as JSON to stderr")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *action == "" || *requester == "" {
		fmt.Fprintln(stderr, "outis gate: -action and -requester are required")
		return exitUsage
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "outis gate: -timeout is required")
		return exitUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "outis gate: name the command to run after the flags")
		return exitUsage
	}
	c, ok := client(getenv, stderr)
	if !ok {
		return exitUsage
	}

	r, err := c.Guard(ctx, outis.GuardOptions{
		Action:         *action,
		Requester:      *requester,
		ShowApprovers:  params,
		Summary:        *summary,
		Quorum:         *quorum,
		IdempotencyKey: *idem,
		Wait:           *timeout,
	})
	if code, done := waitFailed(err, stderr); done {
		return code
	}
	if *asJSON {
		printRequest(stderr, stderr, r, true)
	}
	fmt.Fprintf(stderr, "outis gate: %s authorized by %s, running %s\n", r.ID, strings.Join(r.Approvers, ", "), fs.Arg(0))
	return execute(fs.Args(), r, stdin, stdout, stderr)
}

// execute runs argv with the caller's stdio and returns its exit code.
func execute(argv []string, r *outis.Request, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = append(os.Environ(), "OUTIS_REQUEST_ID="+r.ID, "OUTIS_OPERATION_HASH="+r.OperationHash)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "outis gate: %v\n", err)
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return exitNotFound
		}
		return exitCannotExec
	}

	// A SIGTERM aimed at gate (a CI cancel, a pod shutdown) reaches the command too.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()

	err := cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exitErr.ExitCode()
	default:
		fmt.Fprintf(stderr, "outis gate: %v\n", err)
		return exitError
	}
}

// waitFailed maps an error from WaitFor or Guard to an exit code and message.
func waitFailed(err error, stderr io.Writer) (int, bool) {
	if err == nil {
		return 0, false
	}
	var notAuth *outis.NotAuthorizedError
	var timeout *outis.WaitTimeoutError
	switch {
	case errors.As(err, &notAuth):
		fmt.Fprintf(stderr, "outis: request %s was %s; not running anything\n", notAuth.RequestID, notAuth.Outcome)
		return exitNotAuthorized, true
	case errors.As(err, &timeout):
		fmt.Fprintf(stderr, "outis: request %s is still pending after %s; resume with \"outis wait %s\"\n", timeout.RequestID, timeout.Timeout, timeout.RequestID)
		return exitTimeout, true
	case errors.Is(err, outis.ErrTimeoutRequired), errors.Is(err, outis.ErrTimeoutTooLong):
		fmt.Fprintln(stderr, err)
		return exitUsage, true
	default:
		fmt.Fprintln(stderr, err)
		return exitError, true
	}
}

func cmdVerify(args []string, getenv env, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("verify-webhook", stderr)
	secretEnv := fs.String("secret-env", "OUTIS_WEBHOOK_SECRET", "the environment variable holding the endpoint's secret")
	tolerance := fs.Duration("tolerance", outis.DefaultWebhookTolerance, "how far the signed timestamp may be from now")
	headers := headerFlag{}
	fs.Var(headers, "header", `a delivery header as "Name: value", repeatable`)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	secret := getenv(*secretEnv)
	if secret == "" {
		fmt.Fprintf(stderr, "outis verify-webhook: %s isn't set\n", *secretEnv)
		return exitUsage
	}
	body, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		fmt.Fprintf(stderr, "outis verify-webhook: read body: %v\n", err)
		return exitError
	}
	ev, err := outis.VerifyWebhook(body, http.Header(headers), secret, &outis.VerifyOptions{Tolerance: *tolerance})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ev); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	return exitOK
}

func cmdHash(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hash", stderr)
	params := paramFlag{}
	action := fs.String("action", "", "the action (required)")
	fs.Var(params, "param", "a param as key=value, repeatable")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *action == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "outis hash: -action is required, and nothing else")
		return exitUsage
	}
	fmt.Fprintln(stdout, outis.OperationHash(*action, params))
	return exitOK
}

func printRequest(stdout, stderr io.Writer, r *outis.Request, asJSON bool) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		return exitOK
	}
	outcome := string(r.Outcome)
	if outcome == "" {
		outcome = "pending"
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", r.ID, r.State, outcome, r.OperationHash)
	if len(r.Approvers) > 0 {
		fmt.Fprintf(stdout, "approvers\t%s\n", strings.Join(r.Approvers, ", "))
	}
	return exitOK
}
