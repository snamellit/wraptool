package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/pool"
)

func TestSelectRuntimeSbx(t *testing.T) {
	root := t.TempDir()
	for _, has := range []bool{true, false} {
		old := hasGuix
		hasGuix = func() bool { return has }
		oldSbx := hasSbx
		hasSbx = func() bool { return true }
		for _, mode := range []string{"", "auto"} {
			rt, err := selectRuntime(mode, root)
			if err != nil || rt == runtimeSbx {
				t.Errorf("mode %q hasGuix=%v: got %v, %v (auto must never pick sbx)", mode, has, rt, err)
			}
		}
		hasGuix, hasSbx = old, oldSbx
	}
	rt, err := selectRuntime("sbx", root)
	if err != nil || rt != runtimeSbx || rt.String() != "sbx" {
		t.Errorf("explicit sbx: %v %v", rt, err)
	}
	_, err = selectRuntime("bogus", root)
	if err == nil {
		t.Fatal("want error")
	}
	for _, w := range []string{"auto", "guix", "devcontainer", "sbx"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q lacks %q", err, w)
		}
	}
}

var sbxNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

func TestSbxSandboxName(t *testing.T) {
	first, second := sbxSandboxName("/work/Proj"), sbxSandboxName("/work/Proj")
	if first != second {
		t.Error("not deterministic")
	}
	if sbxSandboxName("/a/app") == sbxSandboxName("/b/app") {
		t.Error("same basename must differ by root hash")
	}
	cases := map[string]string{
		"/work/My Project.v2": "wt-my-project-v2-",
		"/work/ÜberProjekt":   "wt-berprojekt-",
		"/work/--lead":        "wt-lead-",
		"/work/a__b":          "wt-a-b-",
		"/":                   "wt-",
		"/work/!!!":           "wt-",
	}
	for root, prefix := range cases {
		n := sbxSandboxName(root)
		if !strings.HasPrefix(n, prefix) || !sbxNameRE.MatchString(n) || len(n) > 63 || strings.Contains(n, "--") {
			t.Errorf("%q -> %q (want prefix %q)", root, n, prefix)
		}
	}
	long := sbxSandboxName("/work/" + strings.Repeat("x", 200))
	if len(long) > 63 || !sbxNameRE.MatchString(long) {
		t.Errorf("long name %q (%d)", long, len(long))
	}
	// Cap lands on a dash: no trailing dash before the hash separator.
	if n := sbxSandboxName("/w/" + strings.Repeat("a", 50) + "-b"); strings.Contains(n, "--") {
		t.Errorf("%q", n)
	}
}

func TestSbxArgvGolden(t *testing.T) {
	const token = "s3cr3t-bearer"
	join := func(a []string) string { return strings.Join(a, " ") }

	create := buildSbxCreateArgs("wt-p-1", "", "/w/p", "/pool/runtime", "/pool/home")
	if got, want := join(create), "create --name wt-p-1 --skills off -e WRAPTOOL_CONTAINER=1 -e WRAPTOOL_CWD=/w/p shell /w/p /pool/runtime:ro /pool/home"; got != want {
		t.Errorf("create = %s", got)
	}
	tmpl := buildSbxCreateArgs("wt-p-1", "ghcr.io/x/y:1", "/w/p", "/pool/runtime", "/pool/home")
	if got, want := join(tmpl), "create --name wt-p-1 -t ghcr.io/x/y:1 --skills off -e WRAPTOOL_CONTAINER=1 -e WRAPTOOL_CWD=/w/p shell /w/p /pool/runtime:ro /pool/home"; got != want {
		t.Errorf("create+template = %s", got)
	}
	if got, want := join(buildSbxPolicyArgs("n", "8717")), "policy allow network --sandbox n localhost:8717"; got != want {
		t.Errorf("policy = %s", got)
	}
	if got, want := join(buildSbxPolicyCheckArgs("n", "localhost:22")), "policy check network --sandbox n localhost:22"; got != want {
		t.Errorf("check = %s", got)
	}
	tty := buildSbxExecArgs("n", true, "ENTRY")
	if got, want := join(tty), "exec -it n bash -lc ENTRY"; got != want {
		t.Errorf("exec tty = %s", got)
	}
	if got, want := join(buildSbxExecArgs("n", false, "ENTRY")), "exec -i n bash -lc ENTRY"; got != want {
		t.Errorf("exec = %s", got)
	}
	for _, args := range [][]string{create, tmpl} {
		for _, a := range args {
			switch a {
			case "--kit", "--env-file", "--static-mcp", "--clone":
				t.Errorf("forbidden flag %s", a)
			}
			if strings.Contains(a, token) {
				t.Errorf("token in argv: %s", a)
			}
		}
		// every -e is NAME=value
		for i, a := range args {
			if a == "-e" && !strings.Contains(args[i+1], "=") {
				t.Errorf("bare -e %s", args[i+1])
			}
		}
	}
}

// sbxPoolLayout builds a pool under the isolated XDG data dir and returns the
// plan input for harness "claude".
func sbxPoolInput(t *testing.T) poolPlanInput {
	t.Helper()
	withIsolatedXDG(t, "XDG_DATA_HOME")
	key, err := pool.HostPlatformKey()
	if err != nil {
		t.Skip("unsupported host platform")
	}
	return poolPlanInput{
		Harness: "claude", PoolLockable: true,
		PointerPath: pool.PointerFile(key, "claude"),
		RuntimeDir:  pool.RuntimeDir(), RuntimeBinDir: pool.RuntimeBinDir(), RuntimeGenerationsDir: pool.RuntimeGenerationsDir(),
	}
}

func activateSbxPool(t *testing.T, in poolPlanInput, genID string, genBin bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(in.PointerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.PointerPath, []byte(genID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writePoolGeneration(t, in, genID, true, genBin)
}

func TestResolveSbxPoolPlan(t *testing.T) {
	in := sbxPoolInput(t)
	if p := resolveSbxPoolPlan(in); p.err == nil || !strings.Contains(p.err.Error(), "wraptool harness install claude") {
		t.Errorf("no pointer: %v", p.err)
	}
	blank := in
	activateSbxPool(t, in, "  ", false)
	if p := resolveSbxPoolPlan(blank); p.err == nil || !strings.Contains(p.err.Error(), "harness install") {
		t.Errorf("blank pointer: %v", p.err)
	}
	activateSbxPool(t, in, "gen1", false)
	if p := resolveSbxPoolPlan(in); p.err == nil || !strings.Contains(p.err.Error(), "wraptool harness sync") {
		t.Errorf("dangling: %v", p.err)
	}
	activateSbxPool(t, in, "gen1", true)
	p := resolveSbxPoolPlan(in)
	if p.err != nil || p.runtimeDir != in.RuntimeDir || !slices.Equal(p.pathDirs, []string{in.RuntimeBinDir}) {
		t.Errorf("active: %+v", p)
	}
	nl := in
	nl.PoolLockable = false
	if p := resolveSbxPoolPlan(nl); p.err == nil || !strings.Contains(p.err.Error(), "unsupported on --runtime=sbx") {
		t.Errorf("non-lockable: %v", p.err)
	}
}

func TestSbxStateRoundTripAndDrift(t *testing.T) {
	withIsolatedXDG(t, "XDG_DATA_HOME")
	st := sbxState{Name: "wt-a-1", Agent: "shell", Template: "t", Mounts: []string{"/w", "/r:ro", "/h"},
		EnvNames: []string{"A", "B"}, Skills: "off", PinholeResource: "localhost:8717", WraptoolVersion: "v1"}
	if _, found, err := readSbxState(st.Name); found || err != nil {
		t.Fatalf("absent: %v %v", found, err)
	}
	if err := writeSbxState(st); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(sbxStatePath(st.Name))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v %v", fi, err)
	}
	if !strings.HasPrefix(sbxStatePath(st.Name), paths_DataDirForTest()) {
		t.Errorf("state path %s outside the isolated data dir", sbxStatePath(st.Name))
	}
	got, found, err := readSbxState(st.Name)
	if err != nil || !found || !slices.Equal(got.Mounts, st.Mounts) || got.WraptoolVersion != "v1" {
		t.Fatalf("round trip: %+v %v %v", got, found, err)
	}
	if err := os.WriteFile(sbxStatePath(st.Name), []byte(`{"name":"x","bogus":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSbxState(st.Name); err == nil {
		t.Error("unknown field accepted")
	}
	if err := removeSbxState(st.Name); err != nil {
		t.Fatal(err)
	}
	if err := removeSbxState(st.Name); err != nil {
		t.Errorf("second remove: %v", err)
	}

	mutate := map[string]func(*sbxState){
		"agent":       func(s *sbxState) { s.Agent = "claude" },
		"template":    func(s *sbxState) { s.Template = "other" },
		"mounts":      func(s *sbxState) { s.Mounts = []string{"/w", "/r", "/h"} },
		"environment": func(s *sbxState) { s.EnvNames = []string{"A"} },
		"skills":      func(s *sbxState) { s.Skills = "readonly" },
	}
	for field, m := range mutate {
		other := st
		m(&other)
		if d := sbxStateDrift(st, other); !slices.Equal(d, []string{field}) {
			t.Errorf("%s: drift = %v", field, d)
		}
	}
	other := st
	other.WraptoolVersion = "v2"
	other.PinholeResource = "localhost:1"
	if d := sbxStateDrift(st, other); len(d) != 0 {
		t.Errorf("version/pinhole must not drift: %v", d)
	}
}

// paths_DataDirForTest avoids importing paths twice in the test file's header.
func paths_DataDirForTest() string { return filepath.Dir(filepath.Dir(sbxStatePath("x"))) }

// bringUpFixture prepares an isolated data dir with a live pool and a workspace.
func bringUpFixture(t *testing.T) (in sbxBringUpInput) {
	t.Helper()
	pin := sbxPoolInput(t)
	activateSbxPool(t, pin, "gen1", true)
	return sbxBringUpInput{
		Root: t.TempDir(), Token: "s3cr3t-bearer", Listen: "127.0.0.1:8717", AdminListen: "127.0.0.1:8718",
		Harness: "claude", Template: "", Shell: true,
	}
}

func assertCleanCalls(t *testing.T, f *fakeSbx, token string) {
	t.Helper()
	for _, c := range f.calls {
		for _, kv := range c.env {
			if strings.HasPrefix(kv, "SSH_AUTH_SOCK=") || strings.Contains(kv, token) {
				t.Errorf("env of %q has %q", c.line(), kv)
			}
		}
		if strings.Contains(c.line(), token) {
			t.Errorf("token in argv: %s", c.line())
		}
	}
}

func prefixes(lines []string) []string {
	var p []string
	for _, l := range lines {
		f := strings.Fields(l)
		n := 2
		if f[0] == "policy" {
			n = 3
		}
		p = append(p, strings.Join(f[:n], " "))
	}
	return p
}

func TestBringUpSbxSequencing(t *testing.T) {
	in := bringUpFixture(t)
	name := sbxSandboxName(in.Root)

	t.Run("first run", func(t *testing.T) {
		f := installFakeSbx(t, healthySbxReply(t, ""))
		if err := bringUpSbxContainer(in); err != nil {
			t.Fatal(err)
		}
		want := []string{"create --name", "policy allow network", "policy check network", "policy check network", "exec -it"}
		if got := prefixes(f.lines()); !slices.Equal(got, want) {
			t.Fatalf("sequence = %v\n%v", got, f.lines())
		}
		if !f.calls[4].attach || f.calls[0].attach {
			t.Error("only exec is attached")
		}
		if got := f.calls[1].line(); got != "policy allow network --sandbox "+name+" localhost:8717" {
			t.Errorf("pinhole = %s", got)
		}
		if f.calls[2].line() != "policy check network --sandbox "+name+" localhost:8718" || !strings.HasSuffix(f.calls[3].line(), "localhost:22") {
			t.Errorf("probes = %v", f.lines()[2:4])
		}
		if !strings.Contains(f.calls[4].line(), "bash -lc ") {
			t.Errorf("exec = %s", f.calls[4].line())
		}
		assertCleanCalls(t, f, in.Token)
		cfg, err := os.ReadFile(filepath.Join(in.Root, ".mcp.json"))
		if err != nil || !strings.Contains(string(cfg), "host.docker.internal:8717/sse") || !strings.Contains(string(cfg), in.Token) {
			t.Errorf(".mcp.json: %v %s", err, cfg)
		}
		if _, found, _ := readSbxState(name); !found {
			t.Error("state file not written")
		}
	})

	t.Run("second run does not create", func(t *testing.T) {
		f := installFakeSbx(t, healthySbxReply(t, name))
		in2 := in
		in2.Exists = true
		if err := bringUpSbxContainer(in2); err != nil {
			t.Fatal(err)
		}
		want := []string{"policy allow network", "policy check network", "policy check network", "exec -it"}
		if got := prefixes(f.lines()); !slices.Equal(got, want) {
			t.Fatalf("sequence = %v", got)
		}
		assertCleanCalls(t, f, in.Token)
	})

	t.Run("non-tty uses -i", func(t *testing.T) {
		f := installFakeSbx(t, healthySbxReply(t, name))
		sbxStdinIsTTY = func() bool { return false }
		in2 := in
		in2.Exists = true
		if err := bringUpSbxContainer(in2); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(f.calls[len(f.calls)-1].line(), "exec -i ") {
			t.Errorf("%v", f.lines())
		}
	})

	t.Run("no shell issues no exec", func(t *testing.T) {
		f := installFakeSbx(t, healthySbxReply(t, name))
		in2 := in
		in2.Exists, in2.Shell = true, false
		if err := bringUpSbxContainer(in2); err != nil {
			t.Fatal(err)
		}
		for _, c := range f.calls {
			if c.attach || c.argv[0] == "exec" {
				t.Errorf("exec issued: %s", c.line())
			}
		}
	})

	t.Run("drift refuses with no sbx call", func(t *testing.T) {
		f := installFakeSbx(t, healthySbxReply(t, name))
		in2 := in
		in2.Exists, in2.Template = true, "different:template"
		err := bringUpSbxContainer(in2)
		if err == nil || !strings.Contains(err.Error(), "wraptool down --rm") || !strings.Contains(err.Error(), "template") {
			t.Fatalf("err = %v", err)
		}
		if len(f.calls) != 0 {
			t.Errorf("sbx called on drift: %v", f.lines())
		}
	})

	t.Run("missing state refuses with no sbx call", func(t *testing.T) {
		f := installFakeSbx(t, healthySbxReply(t, name))
		if err := removeSbxState(name); err != nil {
			t.Fatal(err)
		}
		in2 := in
		in2.Exists = true
		err := bringUpSbxContainer(in2)
		if err == nil || !strings.Contains(err.Error(), "wraptool down --rm") || len(f.calls) != 0 {
			t.Fatalf("err = %v calls = %v", err, f.lines())
		}
	})

	t.Run("policy allow failure names balanced", func(t *testing.T) {
		installFakeSbx(t, func(l string) (string, error) {
			if strings.HasPrefix(l, "policy allow") {
				return "", os.ErrInvalid
			}
			return "", nil
		})
		err := bringUpSbxContainer(in)
		if err == nil || !strings.Contains(err.Error(), "sbx policy init balanced") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestProbeSbxLoopbackAllowedWarns(t *testing.T) {
	installFakeSbx(t, func(l string) (string, error) {
		if strings.HasSuffix(l, "localhost:22") {
			return readSbxFixture(t, "policy_check_allowed.txt"), nil
		}
		return "garbage", nil
	})
	probeSbxLoopback("n", "127.0.0.1:8718") // must not panic or fail; output goes to stderr
	a, d := classifySbxProbe(readSbxFixture(t, "policy_check_allowed.txt"))
	a2, d2 := classifySbxProbe(readSbxFixture(t, "policy_check_denied.txt"))
	a3, d3 := classifySbxProbe("???")
	if !a || d || a2 || !d2 || a3 || d3 {
		t.Error("probe classification")
	}
}
