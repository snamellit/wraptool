package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pti/wraptool/internal/config"
)

// Preflight for `wraptool up --runtime=sbx` (design/harness/docker-sandbox-runtime.md
// §7, with the §16 rulings R1–R9). Every parser of `sbx` output here fails
// closed: the formats are documented examples, not captures.

// hasSbx reports whether the sbx CLI is on PATH. A var, like hasGuix, so tests
// stub it.
var hasSbx = func() bool { _, err := exec.LookPath("sbx"); return err == nil }

// sbxRunner is the single process boundary for every `sbx` invocation.
// argv excludes the binary name; env is always the minimal environment built by
// sbxEnv. Tests replace sbxRun with a fake that records both.
type sbxRunner interface {
	// Capture runs sbx and returns its stdout and stderr separately.
	Capture(argv, env []string) (stdout, stderr string, err error)
	// Attach runs sbx with the caller's stdin/stdout/stderr attached.
	Attach(argv, env []string) error
}

type execSbxRunner struct{}

// orEmptyEnv guards the exec.Cmd rule that a nil Env inherits the whole parent
// environment (including SSH_AUTH_SOCK): a nil env becomes an empty one.
func orEmptyEnv(env []string) []string {
	if env == nil {
		return []string{}
	}
	return env
}

func (execSbxRunner) Capture(argv, env []string) (string, string, error) {
	cmd := exec.CommandContext(context.Background(), "sbx", argv...)
	cmd.Env = orEmptyEnv(env)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func (execSbxRunner) Attach(argv, env []string) error {
	cmd := exec.CommandContext(context.Background(), "sbx", argv...)
	cmd.Env = orEmptyEnv(env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// sbxRun is the seam every sbx call goes through.
var sbxRun sbxRunner = execSbxRunner{}

// sbxCapture and sbxAttach are the only entry points to sbxRun: they apply the
// minimal environment so no caller can forget to.
func sbxCapture(argv []string) (stdout, stderr string, err error) {
	return sbxRun.Capture(argv, sbxEnv(os.Environ()))
}

func sbxAttach(argv []string) error {
	return sbxRun.Attach(argv, sbxEnv(os.Environ()))
}

// sbxEnvNames and sbxEnvPrefixes are the R6 allowlist. SSH_AUTH_SOCK is
// deliberately absent: sbx forwards the agent of the client that spawns it, so
// not passing it is what closes that path (D9).
var (
	sbxEnvNames = map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TMPDIR": true,
		"TERM": true, "COLORTERM": true, "LANG": true, "DBUS_SESSION_BUS_ADDRESS": true,
	}
	sbxEnvPrefixes = []string{"LC_", "XDG_", "SBX_", "DOCKER_"}
)

// sbxEnv filters parent (os.Environ() shape) down to the allowlist. Names match
// case-sensitively and exactly (LANGUAGE and PATHOLOGY do not pass LANG/PATH);
// prefix families include the underscore.
func sbxEnv(parent []string) []string {
	out := []string{} // never nil: see orEmptyEnv
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if sbxEnvNames[name] {
			out = append(out, kv)
			continue
		}
		for _, p := range sbxEnvPrefixes {
			if strings.HasPrefix(name, p) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

// sbxMinVersion is R1's floor: the release the flag surface was read from.
var sbxMinVersion = [3]int{0, 47, 0}

var sbxVersionRE = regexp.MustCompile(`\d+\.\d+\.\d+`)

// checkSbxVersion extracts the first x.y.z from `sbx version` output (R1).
// Unparseable output or a version below the floor is an error; a newer one
// yields a non-empty notice.
func checkSbxVersion(out string) (version, notice string, err error) {
	version = sbxVersionRE.FindString(out)
	if version == "" {
		return "", "", errors.New("could not find a version number (x.y.z) in `sbx version` output; " +
			"update sbx, or report the output format")
	}
	var v [3]int
	for i, part := range strings.Split(version, ".") {
		v[i], _ = strconv.Atoi(part)
	}
	want := fmt.Sprintf("%d.%d.%d", sbxMinVersion[0], sbxMinVersion[1], sbxMinVersion[2])
	for i := range v {
		if v[i] < sbxMinVersion[i] {
			return version, "", fmt.Errorf("sbx %s is older than the minimum %s; update sbx", version, want)
		}
		if v[i] > sbxMinVersion[i] {
			return version, fmt.Sprintf("notice: sbx %s is newer than %s, the release this runtime was written against", version, want), nil
		}
	}
	return version, "", nil
}

// sbxModelProviders is D8/R3's allowed set of global service secrets.
var sbxModelProviders = map[string]bool{"anthropic": true, "openai": true, "google": true, "cursor": true}

// sbxSecretEntry identifies a global secret by scope/type/name. The SECRET
// column is never kept.
type sbxSecretEntry struct{ Scope, Type, Name string }

func (e sbxSecretEntry) String() string { return e.Scope + "/" + e.Type + "/" + e.Name }

// classifySbxSecrets parses `sbx secret ls --global` (documented table
// SCOPE TYPE NAME SECRET; further columns are tolerated) into allowed and
// offending entries. A header with no rows means no secrets. Anything else the
// parser does not recognise is a parse error — callers treat it as a refusal:
// empty or whitespace-only output (sbx may have written the table elsewhere),
// no header, a row too short to name scope/type/name, a row whose field count
// differs from the header's, and any row whose scope is not exactly "(global)"
// (the command asked for --global, so such a row is output we do not
// understand, including trailing prose). The error text never quotes the input.
func classifySbxSecrets(out string) (allowed, offending []sbxSecretEntry, err error) {
	if strings.TrimSpace(out) == "" {
		return nil, nil, errors.New("`sbx secret ls --global` printed nothing (cannot tell whether secrets exist)")
	}
	var headerSeen bool
	var headerLen int
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if !headerSeen {
			if len(f) >= 3 && f[0] == "SCOPE" && f[1] == "TYPE" && f[2] == "NAME" {
				headerSeen = true
				headerLen = len(f)
				continue
			}
			return nil, nil, errors.New("`sbx secret ls --global` output has no `SCOPE TYPE NAME` header (format not recognised)")
		}
		if len(f) < 3 {
			return nil, nil, fmt.Errorf("`sbx secret ls --global` output line %d has fewer than three columns (format not recognised)", i+1)
		}
		// A blank cell (say NAME) shifts every later column left, which would
		// put the masked secret value in the name slot; a differing field count
		// is therefore a parse error, reported by line number only.
		if len(f) != headerLen {
			return nil, nil, fmt.Errorf("`sbx secret ls --global` output line %d has %d fields but the header has %d (format not recognised)", i+1, len(f), headerLen)
		}
		e := sbxSecretEntry{Scope: f[0], Type: f[1], Name: f[2]}
		if e.Scope != "(global)" {
			return nil, nil, fmt.Errorf("`sbx secret ls --global` output line %d has a scope other than `(global)` (format not recognised)", i+1)
		}
		if e.Type == "service" && sbxModelProviders[e.Name] {
			allowed = append(allowed, e)
		} else {
			offending = append(offending, e)
		}
	}
	return allowed, offending, nil
}

// sbxSecretsRefusal builds the D8 refusal naming each offender and both
// remedies (§6). Pure; entries carry no secret material.
func sbxSecretsRefusal(offending []sbxSecretEntry) error {
	var b strings.Builder
	b.WriteString("refusing to start: global sbx secrets that are not model-provider keys would be injected by the sandbox proxy into every sandbox:\n")
	for _, e := range offending {
		fmt.Fprintf(&b, "  - %s\n", e)
	}
	b.WriteString("remove each with `sbx secret rm <name>`, or re-scope it to the sandboxes that should have it with `--sandbox`; " +
		"or pass --allow-sbx-secrets to accept this")
	return errors.New(b.String())
}

// sbxNotSignedIn is the one signed-out predicate (R2), shared by the preflight
// and `down`; it deliberately ignores the exit status.
func sbxNotSignedIn(stdout, stderr string) bool {
	return strings.Contains(strings.ToLower(stdout+"\n"+stderr), "not authenticated")
}

// sbxPreflightResult is what the preflight learned that bringUp needs.
type sbxPreflightResult struct {
	Name    string
	Exists  bool // a sandbox named Name is already listed
	Version string
}

// sbxPreflight runs §7's checks in order, before the server is touched. The
// `sbx ls --quiet` call doubles as sign-in detection (R2) and existence lookup.
func sbxPreflight(cfg *config.Config, root string, allowSecrets bool) (sbxPreflightResult, error) {
	var res sbxPreflightResult
	if cfg.MCP.Transport != "sse" {
		return res, fmt.Errorf("--runtime=sbx needs mcp.transport: sse (the config has %q); a unix socket cannot cross the VM boundary", cfg.MCP.Transport)
	}
	if !hasSbx() {
		return res, errors.New("--runtime=sbx needs the `sbx` CLI (Docker Sandboxes) on PATH; install it, or use --runtime=guix or --runtime=devcontainer")
	}

	out, errOut, err := sbxCapture([]string{"version"})
	if err != nil {
		return res, fmt.Errorf("running `sbx version`: %w: %s", err, strings.TrimSpace(errOut))
	}
	ver, notice, err := checkSbxVersion(out)
	if err != nil {
		return res, err
	}
	res.Version = ver
	fmt.Printf("sbx version: %s\n", ver)
	if notice != "" {
		fmt.Println(notice)
	}

	res.Name = sbxSandboxName(root)
	out, errOut, err = sbxCapture([]string{"ls", "--quiet"})
	if sbxNotSignedIn(out, errOut) {
		return res, errors.New("sbx is not signed in; run `sbx login`")
	}
	if err != nil {
		return res, fmt.Errorf("running `sbx ls --quiet`: %w: %s", err, strings.TrimSpace(errOut+out))
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == res.Name {
			res.Exists = true
		}
	}

	out, _, err = sbxCapture([]string{"secret", "ls", "--global"})
	if err != nil {
		// Deliberately no output in this error: this is the one command whose
		// output can carry secret material.
		return res, fmt.Errorf("running `sbx secret ls --global` failed (%v); run it yourself to see why", err)
	}
	_, offending, perr := classifySbxSecrets(out)
	switch {
	case perr != nil && !allowSecrets:
		return res, fmt.Errorf("%w; refusing because global secrets cannot be checked (--allow-sbx-secrets overrides)", perr)
	case perr != nil:
		fmt.Printf("warning: --allow-sbx-secrets given; global sbx secrets were not checked: %v\n", perr)
	case len(offending) > 0 && !allowSecrets:
		return res, sbxSecretsRefusal(offending)
	case len(offending) > 0:
		names := make([]string, len(offending))
		for i, e := range offending {
			names[i] = e.String()
		}
		sort.Strings(names)
		fmt.Printf("warning: --allow-sbx-secrets given; the sandbox proxy will inject these global secrets: %s\n", strings.Join(names, ", "))
	}

	out, errOut, err = sbxCapture([]string{"mcp", "ls", "--quiet"})
	if err != nil {
		return res, fmt.Errorf("running `sbx mcp ls --quiet`: %w: %s", err, strings.TrimSpace(errOut))
	}
	var servers []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			servers = append(servers, s)
		}
	}
	if len(servers) > 0 {
		return res, fmt.Errorf("refusing to start: sbx has host MCP servers registered (%s), which a sandbox's gateway could attach; "+
			"remove each with `sbx mcp rm <name>`", strings.Join(servers, ", "))
	}
	return res, nil
}
