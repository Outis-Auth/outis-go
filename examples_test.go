package outis

import (
	"os"
	"os/exec"
	"testing"
)

// TestExamplesCompile builds the examples, which have no tests of their own.
// The Temporal one sits behind a build tag and isn't part of this module.
func TestExamplesCompile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command on PATH")
	}
	cmd := exec.Command("go", "vet", "./examples/...")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("examples don't compile: %v\n%s", err, out)
	}
}
