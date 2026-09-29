package main

import (
	"bytes"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// goTool runs the go command in dir without the network or a workspace.
func goTool(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command on PATH")
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOFLAGS=-mod=mod", "GOSUMDB=off", "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// starterModule makes a temp module that requires this one through a replace.
func starterModule(t *testing.T, extra string) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mod := "module example.com/starter\n\ngo 1.25.5\n\nrequire github.com/outis-auth/outis-go v0.0.0\n" + extra +
		"\nreplace github.com/outis-auth/outis-go => " + root + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInitWorker_Go(t *testing.T) {
	dir := starterModule(t, "")
	code, out, stderr := runCLI(nil, "", "init", "worker", "-runtime", "go", "-dir", dir)
	if code != exitOK || !strings.Contains(out, "main.go") {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
	if vet, err := goTool(t, dir, "vet", "."); err != nil {
		t.Fatalf("go vet on the starter: %v\n%s", err, vet)
	}

	before, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // mine\n"), 0o644)
	code, _, stderr = runCLI(nil, "", "init", "worker", "-dir", dir)
	if code == exitOK || !strings.Contains(stderr, "already exists") {
		t.Fatalf("overwrote main.go: exit %d, %s", code, stderr)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "main.go")); string(after) != "package main // mine\n" || len(before) == 0 {
		t.Error("an existing main.go changed")
	}
}

func TestInitWorker_TemporalGo(t *testing.T) {
	dir := starterModule(t, "require go.temporal.io/sdk v1.49.0\n")
	code, _, stderr := runCLI(nil, "", "init", "worker", "-runtime", "temporal-go", "-dir", dir)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// Vetting needs the Temporal SDK, so it runs only when the module cache has it.
	if out, err := goTool(t, dir, "mod", "download", "go.temporal.io/sdk"); err != nil {
		t.Skipf("go.temporal.io/sdk isn't in the module cache: %s", out)
	}
	if vet, err := goTool(t, dir, "vet", "."); err != nil {
		t.Fatalf("go vet on the temporal starter: %v\n%s", err, vet)
	}
}

func TestInitTemplatesAreFormatted(t *testing.T) {
	for rt, name := range runtimes {
		src, err := templates.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := format.Source(src)
		if err != nil {
			t.Fatalf("%s: %v", rt, err)
		}
		if !bytes.Equal(got, src) {
			t.Errorf("%s isn't gofmt clean", rt)
		}
	}
}

func TestInitWorker_Usage(t *testing.T) {
	for _, args := range [][]string{
		{"init"},
		{"init", "worker", "-runtime", "rust"},
		{"init", "worker", "extra"},
	} {
		if code, _, _ := runCLI(nil, "", args...); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	code, _, stderr := runCLI(nil, "", "init", "worker", "-runtime", "node", "-dir", t.TempDir())
	if code != exitUsage || !strings.Contains(stderr, "npx @outis-auth/sdk init worker") {
		t.Errorf("node: exit %d, %s", code, stderr)
	}
}
