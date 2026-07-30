package backend

import "context"

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
