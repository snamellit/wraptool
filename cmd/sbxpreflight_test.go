package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

func readSbxFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "sbx", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sbxCall is one recorded invocation through the fake process boundary.
type sbxCall struct {
	argv   []string
	env    []string
	attach bool
}

func (c sbxCall) line() string { return strings.Join(c.argv, " ") }

// fakeSbx implements sbxRunner. reply maps an argv line to canned stdout and
// an error; unknown commands succeed with empty output.
type fakeSbx struct {
	calls []sbxCall
	reply func(line string) (string, error)
}

func (f *fakeSbx) Capture(argv, env []string) (string, string, error) {
	f.calls = append(f.calls, sbxCall{argv: argv, env: env})
	if f.reply != nil {
		out, err := f.reply(strings.Join(argv, " "))
		if err != nil {
			return out, "stderr-of-" + argv[0], err
		}
		return out, "", nil
	}
	return "", "", nil
}

func (f *fakeSbx) Attach(argv, env []string) error {
	f.calls = append(f.calls, sbxCall{argv: argv, env: env, attach: true})
	return nil
}

func (f *fakeSbx) lines() []string {
	var l []string
	for _, c := range f.calls {
		l = append(l, c.line())
	}
	return l
}

// installFakeSbx swaps the process seam and hasSbx; the returned fake records
// everything. Env of the test process carries SSH_AUTH_SOCK to prove filtering.
func installFakeSbx(t *testing.T, reply func(string) (string, error)) *fakeSbx {
	t.Helper()
	f := &fakeSbx{reply: reply}
	oldRun, oldHas, oldTTY := sbxRun, hasSbx, sbxStdinIsTTY
	sbxRun, hasSbx, sbxStdinIsTTY = f, func() bool { return true }, func() bool { return true }
	t.Cleanup(func() { sbxRun, hasSbx, sbxStdinIsTTY = oldRun, oldHas, oldTTY })
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	return f
}

// healthySbxReply answers every preflight call as a clean host.
func healthySbxReply(t *testing.T, existing string) func(string) (string, error) {
	return func(line string) (string, error) {
		switch line {
		case "version":
			return readSbxFixture(t, "version.txt"), nil
		case "ls --quiet":
			return existing, nil
		case "secret ls --global":
			return "SCOPE TYPE NAME SECRET\n(global) service anthropic sk-x\n", nil
		case "mcp ls --quiet":
			return "\n", nil
		}
		if strings.HasPrefix(line, "policy check") {
			return readSbxFixture(t, "policy_check_denied.txt"), nil
		}
		if strings.HasPrefix(line, "exec ") && strings.Contains(line, " sh -c ") {
			return "Sandbox x started successfully\nnone\n", nil
		}
		return "", nil
	}
}

func TestSbxEnvAllowlist(t *testing.T) {
	parent := []string{
		"SSH_AUTH_SOCK=/x", "ANTHROPIC_API_KEY=k", "GH_TOKEN=t", "WRAPTOOL_TOKEN=w", "AWS_SECRET_ACCESS_KEY=a",
		"PATH=/bin", "HOME=/h", "USER=u", "LOGNAME=u", "TMPDIR=/t", "TERM=xterm", "COLORTERM=truecolor",
		"LANG=C", "LC_ALL=C", "LC_CTYPE=C", "XDG_DATA_HOME=/d", "SBX_NO_TELEMETRY=1", "DOCKER_HOST=unix:///s",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/b",
		// boundary: only literal names and underscore-terminated prefixes pass
		"PATHOLOGY=1", "LANGUAGE=en", "LCX=1", "XDGX=1", "SBX=1", "DOCKER=1", "HOMEBREW=1", "path=lower", "NOEQUALS",
	}
	got := sbxEnv(parent)
	want := map[string]bool{
		"PATH=/bin": true, "HOME=/h": true, "USER=u": true, "LOGNAME=u": true, "TMPDIR=/t": true, "TERM=xterm": true,
		"COLORTERM=truecolor": true, "LANG=C": true, "LC_ALL=C": true, "LC_CTYPE=C": true, "XDG_DATA_HOME=/d": true,
		"SBX_NO_TELEMETRY=1": true, "DOCKER_HOST=unix:///s": true, "DBUS_SESSION_BUS_ADDRESS=unix:path=/b": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, kv := range got {
		if !want[kv] {
			t.Errorf("unexpected survivor %q", kv)
		}
	}
}

func TestSbxEnvNeverNil(t *testing.T) {
	for _, parent := range [][]string{nil, {"SSH_AUTH_SOCK=/x", "FOO=1"}} {
		if got := sbxEnv(parent); got == nil || len(got) != 0 {
			t.Errorf("sbxEnv(%v) = %#v", parent, got)
		}
	}
	if got := orEmptyEnv(nil); got == nil {
		t.Error("orEmptyEnv(nil) is nil")
	}
}

func TestSbxPreflightWanted(t *testing.T) {
	for _, rt := range []containerRuntime{runtimeGuix, runtimeDevcontainer, runtimeSbx} {
		for _, noC := range []bool{false, true} {
			if got, want := sbxPreflightWanted(rt, noC), rt == runtimeSbx && !noC; got != want {
				t.Errorf("rt=%v noContainer=%v: %v", rt, noC, got)
			}
		}
	}
}

func TestCheckSbxVersion(t *testing.T) {
	cases := []struct {
		name, in   string
		wantErr    string
		wantNotice bool
		wantVer    string
	}{
		{"fixture shape (at floor)", readSbxFixture(t, "version.txt"), "", false, "0.47.0"},
		{"older fixture", readSbxFixture(t, "version_old.txt"), "older than", false, "0.45.0"},
		{"bare json-ish shape", "sbx 0.47.0", "", false, "0.47.0"},
		{"below floor by patch-level major", "0.9.99", "older than", false, "0.9.99"},
		{"newer minor", "sbx version: v0.48.1 abc", "", true, "0.48.1"},
		{"newer major", "1.0.0", "", true, "1.0.0"},
		{"unparseable", "sbx dev build", "could not find a version", false, ""},
		{"empty", "", "could not find a version", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ver, notice, err := checkSbxVersion(c.in)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ver != c.wantVer || (notice != "") != c.wantNotice {
				t.Errorf("ver=%q notice=%q", ver, notice)
			}
		})
	}
}

func entryNames(es []sbxSecretEntry) []string {
	var n []string
	for _, e := range es {
		n = append(n, e.String())
	}
	return n
}

// TestClassifySbxSecretsCaptured drives the classifier with real sbx 0.47.0
// output: the no-secrets case is a prose line, not an empty table, and must be
// matched whole — any extra text around it is still a refusal.
func TestClassifySbxSecretsCaptured(t *testing.T) {
	out := readSbxFixture(t, "secret_ls_global.txt")
	allowed, offending, err := classifySbxSecrets(out)
	if err != nil || len(allowed) != 0 || len(offending) != 0 {
		t.Fatalf("captured no-secrets output: allowed=%v offending=%v err=%v", allowed, offending, err)
	}
	for _, variant := range []string{
		"  " + sbxNoGlobalSecrets + "\n\n", // surrounding whitespace only
	} {
		if _, _, err := classifySbxSecrets(variant); err != nil {
			t.Errorf("%q: %v", variant, err)
		}
	}
	for _, variant := range []string{
		sbxNoGlobalSecrets + "\nUse `sbx secret set` to add one.",
		`No secrets found for scope "sandbox:x".`,
		"no secrets found for scope \"(global)\".",
		"No secrets found.",
	} {
		if _, _, err := classifySbxSecrets(variant); err == nil {
			t.Errorf("%q: want refusal", variant)
		}
	}
}

func TestClassifySbxSecretsFixture(t *testing.T) {
	out := readSbxFixture(t, "secret_ls_global_table.txt")
	allowed, offending, err := classifySbxSecrets(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryNames(allowed); len(got) != 1 || got[0] != "(global)/service/anthropic" {
		t.Errorf("allowed = %v", got)
	}
	// github (non-model service) and the registry row refuse.
	if got := entryNames(offending); len(got) != 2 || got[0] != "(global)/service/github" || got[1] != "(global)/registry/ghcr.io" {
		t.Errorf("offending = %v", got)
	}
	msg := sbxSecretsRefusal(offending).Error()
	for _, must := range []string{"(global)/service/github", "(global)/registry/ghcr.io", "sbx secret rm <name>", "--sandbox", "--allow-sbx-secrets"} {
		if !strings.Contains(msg, must) {
			t.Errorf("refusal lacks %q: %s", must, msg)
		}
	}
	for _, secret := range []string{"gho_GCaw4o", "43qy", "sk-ant", "ghp_Zk39dd", "****"} {
		if strings.Contains(msg, secret) {
			t.Errorf("refusal leaks secret column %q", secret)
		}
	}
	// Parse errors never echo the input either.
	_, _, perr := classifySbxSecrets("gho_GCaw4o****...****43qy\n")
	if perr == nil || strings.Contains(perr.Error(), "gho_") {
		t.Errorf("parse error = %v", perr)
	}
}

func TestClassifySbxSecretsShapes(t *testing.T) {
	const hdr = "SCOPE TYPE NAME SECRET\n"
	t.Run("header only", func(t *testing.T) {
		a, o, err := classifySbxSecrets(hdr)
		if err != nil || len(a) != 0 || len(o) != 0 {
			t.Errorf("%v %v %v", a, o, err)
		}
	})
	t.Run("empty output is a parse error", func(t *testing.T) {
		for _, in := range []string{"", "\n", "  \n\t"} {
			if _, _, err := classifySbxSecrets(in); err == nil {
				t.Errorf("%q accepted", in)
			}
		}
	})
	t.Run("foreign scope spellings and trailing prose refuse", func(t *testing.T) {
		for _, row := range []string{"global service github x", "Global service github x", "my-box service github x", "Use sbx secret rm to remove"} {
			if _, _, err := classifySbxSecrets(hdr + "(global) service anthropic x\n" + row + "\n"); err == nil {
				t.Errorf("%q accepted", row)
			}
		}
	})
	t.Run("no header", func(t *testing.T) {
		if _, _, err := classifySbxSecrets("(global) service github x\n"); err == nil {
			t.Error("want parse error")
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, _, err := classifySbxSecrets("Error: something odd happened\n"); err == nil {
			t.Error("want parse error")
		}
	})
	t.Run("short row", func(t *testing.T) {
		if _, _, err := classifySbxSecrets(hdr + "(global) service\n"); err == nil {
			t.Error("want parse error")
		}
	})
	t.Run("unknown extra column", func(t *testing.T) {
		a, _, err := classifySbxSecrets("SCOPE TYPE NAME SECRET EXPIRES\n(global) service openai sk-x never\n")
		if err != nil || len(a) != 1 {
			t.Errorf("%v %v", a, err)
		}
	})
	t.Run("row field count differing from the header is a parse error", func(t *testing.T) {
		const secret = "sk-MASKEDVALUE"
		cases := map[string]string{
			// blank NAME cell: the masked secret shifts into the name column
			"fewer fields": hdr + "(global) service " + secret + "\n",
			"more fields":  hdr + "(global) service openai " + secret + " extra\n",
			"later row":    hdr + "(global) service openai x\n(global) service " + secret + "\n",
			"wider header": "SCOPE TYPE NAME SECRET EXPIRES\n(global) service openai " + secret + "\n",
		}
		for label, in := range cases {
			a, o, err := classifySbxSecrets(in)
			if err == nil || len(a) != 0 || len(o) != 0 {
				t.Errorf("%s: accepted: %v %v %v", label, a, o, err)
				continue
			}
			for _, leak := range []string{secret, "openai", "service", "(global)"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("%s: error echoes row text %q: %v", label, leak, err)
				}
			}
			if !strings.Contains(err.Error(), "line ") {
				t.Errorf("%s: error names no line: %v", label, err)
			}
		}
	})
	t.Run("extra column on header and every row parses", func(t *testing.T) {
		a, o, err := classifySbxSecrets("SCOPE TYPE NAME SECRET EXPIRES\n(global) service openai sk-x never\n(global) registry ghcr.io tok never\n")
		if err != nil || len(a) != 1 || len(o) != 1 {
			t.Errorf("%v %v %v", a, o, err)
		}
	})
	t.Run("sandbox scoped row refuses", func(t *testing.T) {
		if _, _, err := classifySbxSecrets(hdr + "my-box service github x\n"); err == nil {
			t.Error("want parse error")
		}
	})
	t.Run("every model provider allowed", func(t *testing.T) {
		for _, n := range []string{"anthropic", "openai", "google", "cursor"} {
			a, o, err := classifySbxSecrets(hdr + "(global) service " + n + " x\n")
			if err != nil || len(a) != 1 || len(o) != 0 {
				t.Errorf("%s: %v %v %v", n, a, o, err)
			}
		}
	})
	t.Run("non-model services refused", func(t *testing.T) {
		for _, n := range []string{"github", "copilot", "groq", "xai"} {
			_, o, _ := classifySbxSecrets(hdr + "(global) service " + n + " x\n")
			if len(o) != 1 {
				t.Errorf("%s not refused", n)
			}
		}
	})
	t.Run("model name with other type is an offender", func(t *testing.T) {
		_, o, _ := classifySbxSecrets(hdr + "(global) generic anthropic x\n")
		if len(o) != 1 {
			t.Errorf("offenders = %v", o)
		}
	})
}

func sbxTestCfg() *config.Config {
	return &config.Config{MCP: config.MCPConfig{Transport: "sse", Listen: "127.0.0.1:8717"}}
}

func TestSbxNotSignedIn(t *testing.T) {
	cases := []struct {
		name, out, errOut string
		want              bool
	}{
		{"stdout only", "Error: not authenticated", "", true},
		{"stderr only", "", "not authenticated; run sbx login", true},
		{"mixed case", "You are Not Authenticated to Docker", "", true},
		{"mixed case stderr", "", "NOT AUTHENTICATED", true},
		{"absent", "wt-a-1234\n", "some warning", false},
		{"empty", "", "", false},
		{"split across streams is not a match", "not", "authenticated", false},
	}
	for _, c := range cases {
		if got := sbxNotSignedIn(c.out, c.errOut); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestSbxPreflight(t *testing.T) {
	root := "/work/proj"
	name := sbxSandboxName(root)

	t.Run("healthy, sandbox absent", func(t *testing.T) {
		f := installFakeSbx(t, healthySbxReply(t, "other\n"))
		res, err := sbxPreflight(sbxTestCfg(), root, false)
		if err != nil || res.Exists || res.Name != name {
			t.Fatalf("%+v %v", res, err)
		}
		want := []string{"version", "ls --quiet", "secret ls --global", "mcp ls --quiet"}
		if got := f.lines(); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("calls = %v", got)
		}
	})
	t.Run("healthy, sandbox listed", func(t *testing.T) {
		installFakeSbx(t, healthySbxReply(t, "x\n"+name+"\n"))
		res, err := sbxPreflight(sbxTestCfg(), root, false)
		if err != nil || !res.Exists {
			t.Fatalf("%+v %v", res, err)
		}
	})
	refuse := func(t *testing.T, cfg *config.Config, reply func(string) (string, error), has bool, allow bool, want ...string) {
		t.Helper()
		installFakeSbx(t, reply)
		hasSbx = func() bool { return has }
		_, err := sbxPreflight(cfg, root, allow)
		if err == nil {
			t.Fatal("want refusal")
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("error %q lacks %q", err, w)
			}
		}
	}
	t.Run("sbx missing", func(t *testing.T) {
		refuse(t, sbxTestCfg(), healthySbxReply(t, ""), false, false, "`sbx` CLI", "--runtime=guix")
	})
	t.Run("old version", func(t *testing.T) {
		refuse(t, sbxTestCfg(), func(string) (string, error) { return readSbxFixture(t, "version_old.txt"), nil }, true, false, "older than", "update sbx")
	})
	t.Run("not signed in", func(t *testing.T) {
		h := healthySbxReply(t, "")
		refuse(t, sbxTestCfg(), func(l string) (string, error) {
			if l == "ls --quiet" {
				return "You are not authenticated to Docker", errors.New("exit 1")
			}
			return h(l)
		}, true, false, "sbx login")
	})
	t.Run("ls failure reported verbatim", func(t *testing.T) {
		h := healthySbxReply(t, "")
		refuse(t, sbxTestCfg(), func(l string) (string, error) {
			if l == "ls --quiet" {
				return "", errors.New("daemon down")
			}
			return h(l)
		}, true, false, "daemon down")
	})
	secretReply := func(t *testing.T) func(string) (string, error) {
		h := healthySbxReply(t, "")
		return func(l string) (string, error) {
			if l == "secret ls --global" {
				return readSbxFixture(t, "secret_ls_global_table.txt"), nil
			}
			return h(l)
		}
	}
	t.Run("offending secret", func(t *testing.T) {
		refuse(t, sbxTestCfg(), secretReply(t), true, false, "(global)/service/github", "--allow-sbx-secrets", "sbx secret rm")
	})
	t.Run("override lets secrets through", func(t *testing.T) {
		installFakeSbx(t, secretReply(t))
		out := captureStdout(t, func() {
			if _, err := sbxPreflight(sbxTestCfg(), root, true); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, "--allow-sbx-secrets") || !strings.Contains(out, "(global)/service/github") || strings.Contains(out, "gho_") {
			t.Errorf("override line = %q", out)
		}
	})
	t.Run("garbage secret output refuses, override passes", func(t *testing.T) {
		h := healthySbxReply(t, "")
		r := func(l string) (string, error) {
			if l == "secret ls --global" {
				return "weird", nil
			}
			return h(l)
		}
		refuse(t, sbxTestCfg(), r, true, false, "cannot be checked")
		installFakeSbx(t, r)
		if _, err := sbxPreflight(sbxTestCfg(), root, true); err != nil {
			t.Errorf("override should pass: %v", err)
		}
	})
	t.Run("secret ls failure leaks no output", func(t *testing.T) {
		h := healthySbxReply(t, "")
		installFakeSbx(t, func(l string) (string, error) {
			if l == "secret ls --global" {
				return "STDOUT-MARKER", errors.New("exit 1")
			}
			return h(l)
		})
		_, err := sbxPreflight(sbxTestCfg(), root, true)
		if err == nil || strings.Contains(err.Error(), "MARKER") || strings.Contains(err.Error(), "stderr-of") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("mcp server registered", func(t *testing.T) {
		h := healthySbxReply(t, "")
		refuse(t, sbxTestCfg(), func(l string) (string, error) {
			if l == "mcp ls --quiet" {
				return "github-mcp\n", nil
			}
			return h(l)
		}, true, true, "github-mcp", "sbx mcp rm")
	})
	t.Run("unix transport", func(t *testing.T) {
		cfg := sbxTestCfg()
		cfg.MCP.Transport = "unix"
		refuse(t, cfg, healthySbxReply(t, ""), true, false, "mcp.transport: sse")
	})
}

// runUpSbxHarness runs runUp with the server seam stubbed and all XDG dirs
// isolated; returns the error and how often the server seam was hit.
func runUpSbxHarness(t *testing.T, opts upOptions) (int, error) {
	t.Helper()
	origIsInContainer, origServer := isInContainer, ensureServerFn
	isInContainer = func() bool { return false }
	served := 0
	ensureServerFn = func(*config.Config, string) error { served++; return nil }
	t.Cleanup(func() { isInContainer, ensureServerFn = origIsInContainer, origServer })
	withIsolatedXDG(t, "XDG_RUNTIME_DIR")
	withIsolatedXDG(t, "XDG_CONFIG_HOME")
	withIsolatedXDG(t, "XDG_DATA_HOME")
	opts.workspace = t.TempDir()
	opts.configPath = filepath.Join(t.TempDir(), "config.yaml") // scaffolded valid starter config
	opts.harness, opts.shell = "claude", "never"
	err := runUp(opts)
	return served, err
}

// A failing preflight, or a failing pool plan, returns before the server seam.
func TestRunUpSbxFailuresBeforeServer(t *testing.T) {
	f := installFakeSbx(t, nil)
	hasSbx = func() bool { return false }
	served, err := runUpSbxHarness(t, upOptions{runtime: "sbx"})
	if err == nil || !strings.Contains(err.Error(), "`sbx` CLI") || served != 0 || len(f.calls) != 0 {
		t.Fatalf("preflight: err=%v served=%d", err, served)
	}
	installFakeSbx(t, healthySbxReply(t, ""))
	served, err = runUpSbxHarness(t, upOptions{runtime: "sbx"})
	if err == nil || !strings.Contains(err.Error(), "wraptool harness install") || served != 0 {
		t.Fatalf("pool plan: err=%v served=%d", err, served)
	}
}

// --no-container and the guix runtime make no sbx call at all.
func TestRunUpSbxGating(t *testing.T) {
	f := installFakeSbx(t, healthySbxReply(t, ""))
	served, _ := runUpSbxHarness(t, upOptions{runtime: "sbx", noContainer: true})
	if served != 1 || len(f.calls) != 0 {
		t.Errorf("noContainer: served=%d sbx calls=%v", served, f.lines())
	}
	_, _ = runUpSbxHarness(t, upOptions{runtime: "guix", noServer: true})
	if len(f.calls) != 0 {
		t.Errorf("guix runtime called sbx: %v", f.lines())
	}
}
