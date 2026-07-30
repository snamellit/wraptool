package backend

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// recordedRun captures one commandRunner.Run invocation for assertion.
type recordedRun struct {
	name string
	args []string
	env  map[string]string
	dir  string
}

// fakeRunner is the commandRunner test double: it records every call instead
// of executing anything, so tests can assert the exact generated argv.
type fakeRunner struct {
	calls []recordedRun
	err   error
}

func (f *fakeRunner) Run(_ context.Context, name string, args []string, env map[string]string, dir string) error {
	f.calls = append(f.calls, recordedRun{
		name: name,
		args: append([]string(nil), args...),
		env:  env,
		dir:  dir,
	})
	return f.err
}

// stubLookPath installs a lookPath replacement for the duration of a test and
// restores the original on cleanup. found lists the executable names that
// should resolve successfully; anything else reports not-found.
func stubLookPath(found ...string) func() {
	orig := lookPath
	set := make(map[string]bool, len(found))
	for _, name := range found {
		set[name] = true
	}
	lookPath = func(file string) (string, error) {
		if set[file] {
			return "/usr/bin/" + file, nil
		}
		return "", errNotFound(file)
	}
	return func() { lookPath = orig }
}

type notFoundError string

func (e notFoundError) Error() string { return "executable file not found in $PATH: " + string(e) }

func errNotFound(name string) error { return notFoundError(name) }

// TestTailBuffer locks in tailBuffer's "keep the suffix" behavior. Each case
// is designed to fail if the trim in Write were implemented as
// t.buf.Truncate(maxStderrTail) (which discards everything AFTER index
// maxStderrTail, keeping the prefix) instead of t.buf.Next(excess) (which
// discards from the front, keeping the suffix) — the two are opposite
// directions and only one of them is useful for "what did this command say
// right before it failed".
func TestTailBuffer(t *testing.T) {
	t.Run("under the bound passes through unchanged", func(t *testing.T) {
		var tb tailBuffer
		if _, err := tb.Write([]byte("hello")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got := tb.buf.String(); got != "hello" {
			t.Errorf("buf = %q, want %q", got, "hello")
		}
	})

	t.Run("over the bound (across multiple writes) retains the suffix", func(t *testing.T) {
		var tb tailBuffer
		leading := []byte("LEADING-MARKER-")
		filler := bytes.Repeat([]byte("x"), maxStderrTail)
		trailing := []byte("-TRAILING-MARKER")

		for _, chunk := range [][]byte{leading, filler, trailing} {
			if _, err := tb.Write(chunk); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}

		got := tb.buf.String()
		if len(got) > maxStderrTail {
			t.Fatalf("buf len = %d, want <= %d", len(got), maxStderrTail)
		}
		if strings.Contains(got, "LEADING-MARKER") {
			t.Errorf("buf retained the leading marker, want it dropped (len %d)", len(got))
		}
		if !strings.HasSuffix(got, "-TRAILING-MARKER") {
			t.Errorf("buf did not retain the trailing marker (len %d)", len(got))
		}
	})

	t.Run("a single write larger than the bound trims to the bound, keeping the suffix", func(t *testing.T) {
		var tb tailBuffer
		big := append(bytes.Repeat([]byte("a"), maxStderrTail), []byte("TAIL")...)
		if _, err := tb.Write(big); err != nil {
			t.Fatalf("Write: %v", err)
		}

		got := tb.buf.String()
		if len(got) != maxStderrTail {
			t.Fatalf("buf len = %d, want %d", len(got), maxStderrTail)
		}
		if !strings.HasSuffix(got, "TAIL") {
			t.Errorf("buf did not retain the trailing bytes of the oversized write")
		}
	})
}
