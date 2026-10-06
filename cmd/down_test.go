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
			return "", errors.New("boom")
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
	e.assertLog(t, "sbx ls --quiet", "sbx ls --quiet")
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

func TestDownSbxLsFailureSurfacesAndDockerRuns(t *testing.T) {
	e := setupDown(t, true, true)
	e.failOn["ls --quiet"] = true
	err := runDown(e.root, false)
	if err == nil || !strings.Contains(err.Error(), "sbx ls --quiet") || !strings.Contains(err.Error(), "stderr-of-ls") {
		t.Fatalf("err = %v", err)
	}
	e.assertLog(t, "sbx ls --quiet", "docker ps", "docker stop abc def")
}
