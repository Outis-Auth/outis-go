package main

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed templates/*.tmpl
var templates embed.FS

// runtimes maps -runtime to its template. The other SDKs scaffold the rest.
var runtimes = map[string]string{
	"go":          "templates/go.main.go.tmpl",
	"temporal-go": "templates/temporal-go.main.go.tmpl",
}

var elsewhere = map[string]string{
	"node":        "npx @outis/sdk init worker",
	"temporal-ts": "npx @outis/sdk init worker -runtime temporal",
	"python":      "python -m outis init worker",
}

const initHelp = `outis init worker writes a starter worker, main.go, into -dir. It won't
overwrite a file that's already there.

  -runtime go           a service that polls Outis and runs authorized intents
  -runtime temporal-go  a Temporal workflow that waits on Outis, plus the
                        webhook bridge that signals it

For node, temporal-ts or python, use that SDK's own init.

Usage:
  outis init worker [-runtime go|temporal-go] [-dir .]

Flags:
`

func cmdInit(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "worker" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			fmt.Fprint(stdout, initHelp)
			return exitOK
		}
		fmt.Fprint(stderr, "outis init: the only thing to init is a worker\n\n", initHelp)
		return exitUsage
	}
	flags := newFlags("init worker", stderr)
	flags.Usage = func() {
		fmt.Fprint(flags.Output(), initHelp)
		flags.PrintDefaults()
	}
	runtime := flags.String("runtime", "go", "go or temporal-go")
	dir := flags.String("dir", ".", "where to write main.go")
	if code, ok := parse(flags, args[1:]); !ok {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "outis init worker: unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}
	if cmd, ok := elsewhere[*runtime]; ok {
		fmt.Fprintf(stderr, "outis init worker: for %s, run %s\n", *runtime, cmd)
		return exitUsage
	}
	tmpl, ok := runtimes[*runtime]
	if !ok {
		fmt.Fprintf(stderr, "outis init worker: -runtime is go or temporal-go, not %q\n", *runtime)
		return exitUsage
	}

	src, err := templates.ReadFile(tmpl)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	path := filepath.Join(*dir, "main.go")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		fmt.Fprintf(stderr, "outis init worker: %s already exists, and I won't overwrite it\n", path)
		return exitError
	}
	if err != nil {
		fmt.Fprintf(stderr, "outis init worker: %v\n", err)
		return exitError
	}
	_, werr := f.Write(src)
	if err := errors.Join(werr, f.Close()); err != nil {
		fmt.Fprintf(stderr, "outis init worker: %v\n", err)
		return exitError
	}

	deps := "github.com/outis-auth/outis-go"
	if *runtime == "temporal-go" {
		deps += " go.temporal.io/sdk"
	}
	fmt.Fprintf(stdout, "wrote %s\n\nNext:\n  go get %s\n  OUTIS_API_KEY=outis_sk_... OUTIS_INTENT_KEY=... go run .\n", path, deps)
	return exitOK
}
