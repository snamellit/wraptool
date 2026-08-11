package cmd

import (
	"errors"
	"testing"
)

func TestExitCodePlainErrorIsOne(t *testing.T) {
	if code := ExitCode(errors.New("boom")); code != 1 {
		t.Errorf("ExitCode(plain error) = %d, want 1", code)
	}
}

func TestExitCodeZeroCodeExitErrorFallsBackToOne(t *testing.T) {
	// An *ExitError with Code == 0 is not a meaningful "exit 0 with an error"
	// state; ExitCode falls back to 1 rather than silently succeeding.
	if code := ExitCode(&ExitError{Code: 0, Err: errors.New("x")}); code != 1 {
		t.Errorf("ExitCode(zero-code ExitError) = %d, want 1", code)
	}
}

func TestExitCodeWrappedExitError(t *testing.T) {
	err := errors.Join(errors.New("context"), &ExitError{Code: 2})
	if code := ExitCode(err); code != 2 {
		t.Errorf("ExitCode(wrapped) = %d, want 2", code)
	}
}

// TestOtherCommandsStillExitOne proves main.go's exit-code change did not
// alter any pre-existing command's behaviour: `config validate` against a
// missing file still returns a plain (non-*ExitError) error, and ExitCode
// maps that to exit 1 exactly as main.go always did before this change.
func TestOtherCommandsStillExitOne(t *testing.T) {
	_, err := runValidate(t, "/no/such/config.yaml")
	if err == nil {
		t.Fatal("expected an error for a missing config file")
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("config validate must not return an *ExitError, got %#v", exitErr)
	}
	if code := ExitCode(err); code != 1 {
		t.Errorf("ExitCode(config validate error) = %d, want 1", code)
	}
}
