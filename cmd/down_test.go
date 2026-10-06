package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeDocker implements dockerRunner, appending to a log shared with fakeSbx's
// reply so cross-tool ordering is observable.
type fakeDocker struct {
	log    *[]string
	ids    []string
	psErr  error
	runErr error
	runs   [][]string
	psRoot string
}

func (f *fakeDocker) PS(root string) ([]string, error) {
	f.psRoot = root
	*f.log = append(*f.log, "docker ps")
	return f.ids, f.psErr
}

func (f *fakeDocker) Run(args []string) error {
	f.runs = append(f.runs, args)
	*f.log = append(*f.log, "docker "+strings.Join(args, " "))
	return f.runErr
}

type downEnv struct {
	root   string
	name   string
	log    *[]string
	sbx    *fakeSbx
	docker *fakeDocker
	// sbxNames is what `sbx ls --quiet` prints; failOn maps an argv line to an error.
	sbxNames string
	failOn   map[string]bool
	// failOut is the stdout a failing sbx call returns alongside its error.
	failOut string
}

// setupDown stubs hasSbx/hasDocker and both process seams and isolates the data dir.
func setupDown(t *testing.T, sbx, docker bool) *downEnv {
	t.Helper()
	withIsolatedXDG(t, "XDG_DATA_HOME")
	e := &downEnv{root: filepath.Join(t.TempDir(), "proj"), log: new([]string), failOn: map[string]bool{}}
	e.name = sbxSandboxName(e.root)
	e.sbxNames = e.name + "\n"
	e.sbx = &fakeSbx{reply: func(line string) (string, error) {
		*e.log = append(*e.log, "sbx "+line)
		if e.failOn[line] {
			return e.failOut, errors.New("boom")
		}
		if line == "ls --quiet" {
			return e.sbxNames, nil
		}
		return "", nil
	}}
	e.docker = &fakeDocker{log: e.log, ids: []string{"abc", "def"}}
	oldRun, oldSbx, oldDocker, oldDRun := sbxRun, hasSbx, hasDocker, dockerRun
	sbxRun, hasSbx, hasDocker, dockerRun = e.sbx, func() bool { return sbx }, func() bool { return docker }, e.docker
	t.Cleanup(func() { sbxRun, hasSbx, hasDocker, dockerRun = oldRun, oldSbx, oldDocker, oldDRun })
	return e
}

func (e *downEnv) writeState(t *testing.T, pinhole string) {
	t.Helper()
	b, err := json.Marshal(sbxState{Name: e.name, PinholeResource: pinhole})
	if err != nil {
		t.Fatal(err)
	}
	p := sbxStatePath(e.name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *downEnv) stateExists() bool {
	_, err := os.Stat(sbxStatePath(e.name))
	return err == nil
}

func (e *downEnv) assertLog(t *testing.T, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(*e.log, want) {
		t.Errorf("calls:\n got %q\nwant %q", *e.log, want)
	}
}

func TestDownDispatchNeither(t *testing.T) {
	e := setupDown(t, false, false)
	err := runDown(e.root, false)
	if err == nil || !strings.Contains(err.Error(), "sbx") || !strings.Contains(err.Error(), "docker") || !strings.Contains(err.Error(), "nothing") {
		t.Fatalf("err = %v", err)
	}
	e.assertLog(t)
}

func TestDownDispatchDockerOnly(t *testing.T) {
	e := setupDown(t, false, true)
	if err := runDown(e.root, false); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "docker ps", "docker stop abc def")
	if e.docker.psRoot != e.root {
		t.Errorf("ps root = %q", e.docker.psRoot)
	}
	// Byte-identical argv pins.
	if want := []string{"stop", "abc", "def"}; !reflect.DeepEqual(e.docker.runs[0], want) {
		t.Errorf("stop argv = %q", e.docker.runs[0])
	}
	e.docker.runs = nil
	if err := runDown(e.root, true); err != nil {
		t.Fatal(err)
	}
	if want := []string{"rm", "-f", "abc", "def"}; !reflect.DeepEqual(e.docker.runs[0], want) {
		t.Errorf("rm argv = %q", e.docker.runs[0])
	}
}

func TestDownDockerNoneFound(t *testing.T) {
	e := setupDown(t, false, true)
	e.docker.ids = nil
	if err := runDown(e.root, false); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "docker ps")
}

func TestDownDispatchSbxOnly(t *testing.T) {
	e := setupDown(t, true, false)
	if err := runDown(e.root, false); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx stop "+e.name)
}

func TestDownDispatchBoth(t *testing.T) {
	e := setupDown(t, true, true)
	if err := runDown(e.root, false); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx stop "+e.name, "docker ps", "docker stop abc def")
}

func TestDownSandboxRm(t *testing.T) {
	e := setupDown(t, true, false)
	e.writeState(t, "localhost:8717")
	if err := runDown(e.root, true); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx rm --force "+e.name,
		"sbx policy rm network --sandbox "+e.name+" --resource localhost:8717 --force")
	if e.stateExists() {
		t.Error("state file not removed")
	}
}

func TestDownSandboxRmNoPinhole(t *testing.T) {
	e := setupDown(t, true, false)
	e.writeState(t, "")
	if err := runDown(e.root, true); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx rm --force "+e.name)
	if e.stateExists() {
		t.Error("state file not removed")
	}
}

func TestDownStopLeavesState(t *testing.T) {
	e := setupDown(t, true, false)
	e.writeState(t, "localhost:8717")
	if err := runDown(e.root, false); err != nil {
		t.Fatal(err)
	}
	if !e.stateExists() {
		t.Error("plain down must keep state")
	}
}

func TestDownPolicyRmFailureIsNotice(t *testing.T) {
	e := setupDown(t, true, false)
	e.writeState(t, "localhost:8717")
	e.failOn["policy rm network --sandbox "+e.name+" --resource localhost:8717 --force"] = true
	if err := runDown(e.root, true); err != nil {
		t.Fatalf("policy failure must not fail down: %v", err)
	}
	if e.stateExists() {
		t.Error("state should still be removed")
	}
}

func TestDownSbxRmFailureKeepsState(t *testing.T) {
	e := setupDown(t, true, false)
	e.writeState(t, "localhost:8717")
	e.failOn["rm --force "+e.name] = true
	err := runDown(e.root, true)
	if err == nil || !strings.Contains(err.Error(), "rm --force") {
		t.Fatalf("err = %v", err)
	}
	if !e.stateExists() {
		t.Error("state must stay when sbx rm fails")
	}
	e.assertLog(t, "sbx ls --quiet", "sbx rm --force "+e.name)
}

func TestDownSandboxAbsent(t *testing.T) {
	e := setupDown(t, true, false)
	e.sbxNames = ""
	e.writeState(t, "localhost:8717")
	if err := runDown(e.root, false); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "sbx ls --quiet")
	if !e.stateExists() {
		t.Error("state must stay without --rm")
	}
	if err := runDown(e.root, true); err != nil {
		t.Fatal(err)
	}
	// The readable stale state names a rule, so --rm now also drops it.
	e.assertLog(t, "sbx ls --quiet", "sbx ls --quiet",
		"sbx policy rm network --sandbox "+e.name+" --resource localhost:8717 --force")
	if e.stateExists() {
		t.Error("stale state must be removed with --rm")
	}
}

func TestDownNameMatchIsExact(t *testing.T) {
	e := setupDown(t, true, false)
	e.sbxNames = e.name + "-other\n" + e.name[:len(e.name)-1] + "\nother-" + e.name + "\n"
	if err := runDown(e.root, true); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "sbx ls --quiet")
}

func TestDownSandboxFailsDockerStillRuns(t *testing.T) {
	e := setupDown(t, true, true)
	e.failOn["stop "+e.name] = true
	err := runDown(e.root, false)
	if err == nil || !strings.Contains(err.Error(), "sbx stop") {
		t.Fatalf("err = %v", err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx stop "+e.name, "docker ps", "docker stop abc def")
}

func TestDownDockerFailsSandboxStillRuns(t *testing.T) {
	e := setupDown(t, true, true)
	e.docker.runErr = errors.New("daemon down")
	err := runDown(e.root, false)
	if err == nil || !strings.Contains(err.Error(), "docker stop") {
		t.Fatalf("err = %v", err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx stop "+e.name, "docker ps", "docker stop abc def")
}

func (e *downEnv) writeCorruptState(t *testing.T) {
	t.Helper()
	p := sbxStatePath(e.name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDownCorruptStateRmListed(t *testing.T) {
	e := setupDown(t, true, false)
	e.writeCorruptState(t)
	if err := runDown(e.root, true); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx rm --force "+e.name)
	if e.stateExists() {
		t.Error("corrupt state must be removed")
	}
}

func TestDownCorruptStatePlainStop(t *testing.T) {
	e := setupDown(t, true, false)
	e.writeCorruptState(t)
	if err := runDown(e.root, false); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx stop "+e.name)
	if !e.stateExists() {
		t.Error("plain down must not touch state")
	}
}

func TestDownCorruptStateAbsentRm(t *testing.T) {
	e := setupDown(t, true, false)
	e.sbxNames = ""
	e.writeCorruptState(t)
	if err := runDown(e.root, true); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "sbx ls --quiet")
	if e.stateExists() {
		t.Error("corrupt stale state must be removed")
	}
}

func TestDownCorruptStateSbxRmFailureKeepsState(t *testing.T) {
	e := setupDown(t, true, false)
	e.writeCorruptState(t)
	e.failOn["rm --force "+e.name] = true
	if err := runDown(e.root, true); err == nil {
		t.Fatal("expected error")
	}
	if !e.stateExists() {
		t.Error("state must stay when sbx rm fails")
	}
}

func TestDownOutputLines(t *testing.T) {
	// Dev-container literals are those of impl/sbx-runtime-core:cmd/down.go.
	e := setupDown(t, false, true)
	e.docker.ids = nil
	got := captureStdout(t, func() { _ = runDown(e.root, false) })
	if want := "no dev container found for " + e.root + "\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}

	e.docker.ids = []string{"abc", "def"}
	got = captureStdout(t, func() { _ = runDown(e.root, false) })
	if want := "stopping dev container(s) for " + e.root + ": abc def\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	got = captureStdout(t, func() { _ = runDown(e.root, true) })
	if want := "removing dev container(s) for " + e.root + ": abc def\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDownOutputSandboxLines(t *testing.T) {
	e := setupDown(t, true, false)
	e.sbxNames = ""
	got := captureStdout(t, func() { _ = runDown(e.root, false) })
	if want := "no sandbox found for " + e.root + "\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	e.writeState(t, "")
	got = captureStdout(t, func() { _ = runDown(e.root, true) })
	if want := "no sandbox found for " + e.root + "; removed stale state for " + e.name + "\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDownCorruptNoticeText(t *testing.T) {
	e := setupDown(t, true, false)
	e.writeCorruptState(t)
	got := captureStdout(t, func() { _ = runDown(e.root, true) })
	if !strings.Contains(got, "notice: sandbox state file for "+e.name+" is unreadable") ||
		!strings.Contains(got, "sbx policy ls "+e.name) {
		t.Errorf("notice missing: %q", got)
	}
}

func TestDownRmListedNoState(t *testing.T) {
	e := setupDown(t, true, false)
	if err := runDown(e.root, true); err != nil {
		t.Fatal(err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx rm --force "+e.name)
}

func TestDownDockerPSFailure(t *testing.T) {
	e := setupDown(t, true, true)
	e.docker.psErr = errors.New("ps broke")
	err := runDown(e.root, false)
	if err == nil || !strings.Contains(err.Error(), "ps broke") {
		t.Fatalf("err = %v", err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx stop "+e.name, "docker ps")
}

func TestDownCorruptStateAbsentPlainKeepsFile(t *testing.T) {
	e := setupDown(t, true, false)
	e.sbxNames = ""
	e.writeCorruptState(t)
	if err := runDown(e.root, false); err != nil {
		t.Fatal(err)
	}
	if !e.stateExists() {
		t.Error("plain down must keep the file")
	}
}

func TestDownPolicyRmRunsWhenStateRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	e := setupDown(t, true, false)
	e.writeState(t, "localhost:8717")
	dir := filepath.Dir(sbxStatePath(e.name))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	err := runDown(e.root, true)
	if err == nil || !strings.Contains(err.Error(), "removing sandbox state") {
		t.Fatalf("err = %v", err)
	}
	e.assertLog(t, "sbx ls --quiet", "sbx rm --force "+e.name,
		"sbx policy rm network --sandbox "+e.name+" --resource localhost:8717 --force")
}

func TestDownStaleReadableStatePolicyRm(t *testing.T) {
	policy := func(e *downEnv) string {
		return "sbx policy rm network --sandbox " + e.name + " --resource localhost:8717 --force"
	}
	t.Run("with resource", func(t *testing.T) {
		e := setupDown(t, true, false)
		e.sbxNames = ""
		e.writeState(t, "localhost:8717")
		if err := runDown(e.root, true); err != nil {
			t.Fatal(err)
		}
		e.assertLog(t, "sbx ls --quiet", policy(e))
		if e.stateExists() {
			t.Error("stale state must be removed")
		}
	})
	t.Run("policy failure is a notice", func(t *testing.T) {
		e := setupDown(t, true, false)
		e.sbxNames = ""
		e.writeState(t, "localhost:8717")
		e.failOn["policy rm network --sandbox "+e.name+" --resource localhost:8717 --force"] = true
		if err := runDown(e.root, true); err != nil {
			t.Fatal(err)
		}
		if e.stateExists() {
			t.Error("stale state must be removed")
		}
	})
	t.Run("without resource", func(t *testing.T) {
		e := setupDown(t, true, false)
		e.sbxNames = ""
		e.writeState(t, "")
		if err := runDown(e.root, true); err != nil {
			t.Fatal(err)
		}
		e.assertLog(t, "sbx ls --quiet")
		if e.stateExists() {
			t.Error("stale state must be removed")
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		e := setupDown(t, true, false)
		e.sbxNames = ""
		e.writeCorruptState(t)
		if err := runDown(e.root, true); err != nil {
			t.Fatal(err)
		}
		e.assertLog(t, "sbx ls --quiet")
		if e.stateExists() {
			t.Error("stale state must be removed")
		}
	})
}

func TestDownSbxLsNotAuthenticated(t *testing.T) {
	t.Run("no state is a notice", func(t *testing.T) {
		e := setupDown(t, true, true)
		e.failOn["ls --quiet"] = true
		e.failOut = "Error: Not Authenticated"
		var err error
		got := captureStdout(t, func() { err = runDown(e.root, false) })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(got, "sbx is not signed in; sandbox check skipped") || !strings.Contains(got, "sbx login") {
			t.Errorf("notice missing: %q", got)
		}
		e.assertLog(t, "sbx ls --quiet", "docker ps", "docker stop abc def")
	})
	t.Run("state present is an error", func(t *testing.T) {
		e := setupDown(t, true, true)
		e.failOn["ls --quiet"] = true
		e.failOut = "not authenticated"
		e.writeState(t, "")
		err := runDown(e.root, true)
		if err == nil || !strings.Contains(err.Error(), "sbx login") {
			t.Fatalf("err = %v", err)
		}
		if !e.stateExists() {
			t.Error("state must stay")
		}
		e.assertLog(t, "sbx ls --quiet", "docker ps", "docker rm -f abc def")
	})
	t.Run("corrupt state counts as present", func(t *testing.T) {
		e := setupDown(t, true, false)
		e.failOn["ls --quiet"] = true
		e.failOut = "not authenticated"
		e.writeCorruptState(t)
		if err := runDown(e.root, false); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestDownSbxLsFailureSurfacesAndDockerRuns(t *testing.T) {
	e := setupDown(t, true, true)
	e.failOn["ls --quiet"] = true
	err := runDown(e.root, false)
	if err == nil || !strings.Contains(err.Error(), "sbx ls --quiet") || !strings.Contains(err.Error(), "stderr-of-ls") {
		t.Fatalf("err = %v", err)
	}
	e.assertLog(t, "sbx ls --quiet", "docker ps", "docker stop abc def")
}
