package cmd

import (
	"os"
	"os/exec"
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
	if got, want := join(tmpl), "create --name wt-p-1 --template=ghcr.io/x/y:1 --skills off -e WRAPTOOL_CONTAINER=1 -e WRAPTOOL_CWD=/w/p shell /w/p /pool/runtime:ro /pool/home"; got != want {
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
	if d := sbxStateDrift(st, other); len(d) != 0 {
		t.Errorf("version must not drift: %v", d)
	}
	other.PinholeResource = "localhost:1"
	if d := sbxStateDrift(st, other); !slices.Equal(d, []string{"pinhole"}) {
		t.Errorf("pinhole drift = %v", d)
	}
	other.PinholeResource = ""
	if d := sbxStateDrift(st, other); !slices.Equal(d, []string{"pinhole"}) {
		t.Errorf("empty pinhole drift = %v", d)
	}
}

// paths_DataDirForTest avoids importing paths twice in the test file's header.
func paths_DataDirForTest() string { return filepath.Dir(filepath.Dir(sbxStatePath("x"))) }

// bringUpFixture prepares an isolated data dir with a live pool and a workspace.
func bringUpFixture(t *testing.T) (in sbxBringUpInput) {
	t.Helper()
	pin := sbxPoolInput(t)
	activateSbxPool(t, pin, "gen1", true)
	plan, err := sbxPoolPlanForHarness("claude")
	if err != nil {
		t.Fatal(err)
	}
	return sbxBringUpInput{
		Plan: plan,
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

	t.Run("port change refuses, config untouched; same port passes", func(t *testing.T) {
		f := installFakeSbx(t, healthySbxReply(t, name))
		in2 := in
		in2.Exists, in2.Listen = true, "127.0.0.1:9999"
		cfgPath := filepath.Join(in.Root, ".mcp.json")
		before, _ := os.ReadFile(cfgPath)
		err := bringUpSbxContainer(in2)
		if err == nil || !strings.Contains(err.Error(), "port changed") || !strings.Contains(err.Error(), "wraptool down --rm") || len(f.calls) != 0 {
			t.Fatalf("err = %v calls = %v", err, f.lines())
		}
		after, _ := os.ReadFile(cfgPath)
		if string(before) != string(after) {
			t.Error("harness config rewritten on a refused run")
		}
		in2.Listen = in.Listen
		if err := bringUpSbxContainer(in2); err != nil {
			t.Errorf("unchanged port: %v", err)
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

func TestBringUpSbxStalePinholeOnRecreate(t *testing.T) {
	const oldRes = "localhost:9999"
	var lastRunOut string // stdout of the most recent run()
	run := func(t *testing.T, setup func(name string), failRm bool) (*fakeSbx, string, error) {
		t.Helper()
		in := bringUpFixture(t)
		name := sbxSandboxName(in.Root)
		setup(name)
		base := healthySbxReply(t, "")
		f := installFakeSbx(t, func(l string) (string, error) {
			if failRm && strings.HasPrefix(l, "policy rm") {
				return "", os.ErrInvalid
			}
			return base(l)
		})
		var err error
		out := captureStdout(t, func() { err = bringUpSbxContainer(in) })
		lastRunOut = out
		return f, name, err
	}
	hasPrefix := func(f *fakeSbx, p string) bool {
		for _, l := range f.lines() {
			if strings.HasPrefix(l, p) {
				return true
			}
		}
		return false
	}
	writeState := func(t *testing.T, n, res string) {
		t.Helper()
		if err := writeSbxState(sbxState{Name: n, PinholeResource: res}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("different resource removes the old rule before create", func(t *testing.T) {
		f, name, err := run(t, func(n string) { writeState(t, n, oldRes) }, false)
		if err != nil {
			t.Fatal(err)
		}
		lines := f.lines()
		want := "policy rm network --sandbox " + name + " --resource " + oldRes + " --force"
		if len(lines) < 2 || lines[0] != want || !strings.HasPrefix(lines[1], "create --name") {
			t.Fatalf("sequence = %v", lines)
		}
	})
	t.Run("same resource issues no policy rm", func(t *testing.T) {
		f, _, err := run(t, func(n string) { writeState(t, n, "localhost:8717") }, false)
		if err != nil || hasPrefix(f, "policy rm") || !hasPrefix(f, "create --name") {
			t.Fatalf("err = %v lines = %v", err, f.lines())
		}
	})
	t.Run("no state issues no policy rm", func(t *testing.T) {
		f, _, err := run(t, func(string) {}, false)
		if err != nil || hasPrefix(f, "policy rm") || !hasPrefix(f, "create --name") {
			t.Fatalf("err = %v lines = %v", err, f.lines())
		}
	})
	t.Run("corrupt state still creates, with a notice", func(t *testing.T) {
		f, _, err := run(t, func(n string) {
			p := sbxStatePath(n)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false)
		got := lastRunOut
		if err != nil || hasPrefix(f, "policy rm") || !strings.HasPrefix(f.lines()[0], "create --name") {
			t.Fatalf("err = %v lines = %v", err, f.lines())
		}
		if !strings.Contains(got, "previous network rule could not be identified") {
			t.Errorf("notice missing: %q", got)
		}
	})
	t.Run("policy rm failure does not fail up", func(t *testing.T) {
		f, _, err := run(t, func(n string) { writeState(t, n, oldRes) }, true)
		if err != nil || !hasPrefix(f, "policy rm") || !hasPrefix(f, "create --name") {
			t.Fatalf("err = %v lines = %v", err, f.lines())
		}
	})
}

func TestBringUpSbxCreateFailureHintsPolicyInit(t *testing.T) {
	in := bringUpFixture(t)
	installFakeSbx(t, func(l string) (string, error) {
		if strings.HasPrefix(l, "create") {
			return "", os.ErrInvalid
		}
		return "", nil
	})
	err := bringUpSbxContainer(in)
	if err == nil || !strings.Contains(err.Error(), "sbx policy init balanced") || !strings.Contains(err.Error(), "stderr-of-create") {
		t.Fatalf("err = %v", err)
	}
}

func TestSbxEntryConvertsSeparators(t *testing.T) {
	// filepath.ToSlash is a no-op on Linux, so the conversion is injected: a
	// Windows-shaped input proves every path reaches the entry converted.
	toSlash := func(s string) string { return strings.ReplaceAll(s, `\`, "/") }
	got := sbxEntry(`C:\work\proj`, `C:\pool\home`, []string{`C:\pool\runtime\bin`}, toSlash)
	want := sbxPlaceholderUnset + "; " + buildContainerEntry("C:/work/proj", "C:/pool/home", []string{"C:/pool/runtime/bin"})
	if got != want {
		t.Errorf("entry =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, `\`) {
		t.Errorf("backslash in entry: %s", got)
	}
	// Slash-only input is the unwrapped builder behind the placeholder cleanup.
	if got, want := sbxEntry("/w/p", "/h", []string{"/r/bin"}, filepath.ToSlash), sbxPlaceholderUnset+"; "+buildContainerEntry("/w/p", "/h", []string{"/r/bin"}); got != want {
		t.Errorf("entry = %s, want %s", got, want)
	}
}

// TestSbxEntryStripsPlaceholders runs the entry's cleanup prefix in a real
// bash against the environment observed inside an sbx 0.47.0 shell sandbox:
// every proxy placeholder must be gone and ordinary variables untouched.
func TestSbxEntryStripsPlaceholders(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	entry := strings.TrimSuffix(sbxEntry("/w", "/h", nil, filepath.ToSlash), "exec bash -l") + "env"
	cmd := exec.Command(bash, "-c", entry)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/x",
		"ANTHROPIC_API_KEY=proxy-managed",
		"GH_TOKEN=gho_sbxproxymanaged000000000000000000000",
		"XAI_API_KEY=xai-sbxproxymanaged0000", // unobserved service: caught by value
		"MCP_GATEWAY_URL=http://mcp-gateway.docker.internal/mcp",
		"MCP_SENTINEL_TOKEN_NAME=proxy-managed",
		"SSH_AUTH_SOCK=/run/ssh-agent.sock",
		"SSH_AUTH_SOCK_GATEWAY=gateway.docker.internal:3129",
		"SBX_CRED_ANTHROPIC_MODE=none",
		"KEEP_ME=proxy-managed-but-not-exactly",
		"WRAPTOOL_TOKEN=tok",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entry failed: %v\n%s", err, out)
	}
	env := string(out)
	for _, gone := range []string{"ANTHROPIC_API_KEY=", "GH_TOKEN=", "XAI_API_KEY=", "MCP_GATEWAY_URL=", "MCP_SENTINEL_TOKEN_NAME=", "SSH_AUTH_SOCK=", "SSH_AUTH_SOCK_GATEWAY="} {
		if strings.Contains(env, gone) {
			t.Errorf("%s survived the entry:\n%s", gone, env)
		}
	}
	for _, kept := range []string{"SBX_CRED_ANTHROPIC_MODE=none", "KEEP_ME=proxy-managed-but-not-exactly", "WRAPTOOL_TOKEN=tok", "HOME=/h", "WRAPTOOL_CONTAINER=1"} {
		if !strings.Contains(env, kept) {
			t.Errorf("%s missing after the entry:\n%s", kept, env)
		}
	}
}

func TestBringUpSbxRefusalsNameRemovalScope(t *testing.T) {
	in := bringUpFixture(t)
	name := sbxSandboxName(in.Root)
	in.Exists = true
	good := sbxState{
		Name: name, Agent: sbxAgent, Skills: sbxSkillsMode, PinholeResource: "localhost:8717",
		Mounts: []string{
			filepath.ToSlash(in.Root), filepath.ToSlash(in.Plan.runtimeDir) + ":ro", filepath.ToSlash(pool.HomeDir()),
		},
		EnvNames: sbxCreateEnvNames,
	}
	write := func(t *testing.T, st sbxState) {
		t.Helper()
		if err := writeSbxState(st); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T){
		"no state": func(t *testing.T) {
			if err := removeSbxState(name); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable state": func(t *testing.T) {
			p := sbxStatePath(name)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("{bad"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"port change": func(t *testing.T) {
			st := good
			st.PinholeResource = "localhost:1"
			write(t, st)
		},
		"drift": func(t *testing.T) {
			st := good
			st.Template = "other"
			write(t, st)
		},
	}
	for label, setup := range cases {
		t.Run(label, func(t *testing.T) {
			installFakeSbx(t, healthySbxReply(t, name))
			setup(t)
			err := bringUpSbxContainer(in)
			if err == nil || !strings.Contains(err.Error(), "wraptool down --rm") {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(err.Error(), sbxDownRmScope) {
				t.Errorf("refusal lacks the shared scope sentence: %v", err)
			}
			if label == "no state" && !strings.Contains(err.Error(), "failed part-way") {
				t.Errorf("err = %v", err)
			}
		})
	}
}
