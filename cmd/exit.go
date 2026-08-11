package cmd

import "errors"

// ExitError carries a specific process exit status through a cobra RunE
// return value. Every command other than `doctor` keeps today's behaviour
// (any returned error is exit 1, via main.go printing err.Error() and
// exiting 1); doctor is the first command whose exit code carries meaning
// beyond "something went wrong" (0/1/2, see cmd/doctor.go), so it returns an
// *ExitError instead of os.Exit-ing directly, which keeps its RunE testable.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode maps an error returned from the root command's Execute to a
// process exit status. A nil-Err *ExitError (doctor's "the report you just
// read has fail findings" case) exits with its Code and prints nothing extra
// — main.go only prints err.Error() when it is non-empty.
func ExitCode(err error) int {
	var e *ExitError
	if errors.As(err, &e) && e.Code != 0 {
		return e.Code
	}
	return 1
}
