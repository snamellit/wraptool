package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pti/wraptool/internal/buildinfo"
	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/paths"
	"github.com/pti/wraptool/internal/pool"
)

// The Docker Sandboxes runtime (`wraptool up --runtime=sbx`), design/harness/docker-sandbox-runtime.md
// §5. Authority lives in wraptool's own argv (D4): never --kit, --env-file,
// --static-mcp or --clone, and the bearer token only ever lands in the
// harness config file in the workspace.

const (
	sbxAgent      = "shell"
	sbxSkillsMode = "off" // R5
	// sbxNameBaseMax caps the sanitized basename so the whole name stays
	// within 63 characters: "wt-" (3) + base + "-" (1) + 8 hex = 12 + base.
	sbxNameBaseMax = 51
)

// sbxCreateEnvNames are the only environment variables passed at creation, always
// as NAME=value (never a bare name, which would copy the host's value).
var sbxCreateEnvNames = []string{"WRAPTOOL_CONTAINER", "WRAPTOOL_CWD"}

// sbxSandboxName derives the deterministic sandbox name for a worktree:
// wt-<sanitized basename>-<first 8 hex of sha256(root)>. The result is
// lowercase [a-z0-9-], at most 63 characters, starts with a letter, and so
// satisfies sbx create's --name rule for any input; an empty or all-symbol
// basename simply drops that segment.
func sbxSandboxName(root string) string {
	sum := sha256.Sum256([]byte(root))
	hash := hex.EncodeToString(sum[:])[:8]

	var b strings.Builder
	dash := true // suppresses leading and doubled dashes
	for _, r := range strings.ToLower(filepath.Base(root)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	base := b.String()
	if len(base) > sbxNameBaseMax {
		base = base[:sbxNameBaseMax]
	}
	base = strings.Trim(base, "-")
	if base == "" {
		return "wt-" + hash
	}
	return "wt-" + base + "-" + hash
}

// sbxState is the creation-input record compared on re-entry (§5.2, R7).
// WraptoolVersion is recorded for diagnosis but never compared.
type sbxState struct {
	Name            string   `json:"name"`
	Agent           string   `json:"agent"`
	Template        string   `json:"template"`
	Mounts          []string `json:"mounts"`
	EnvNames        []string `json:"env_names"`
	Skills          string   `json:"skills"`
	PinholeResource string   `json:"pinhole_resource"`
	WraptoolVersion string   `json:"wraptool_version"`
}

// sbxStatePath is paths.DataDir()/sandboxes/<name>.json.
func sbxStatePath(name string) string {
	return filepath.Join(paths.DataDir(), "sandboxes", name+".json")
}

// readSbxState returns the stored state; found is false when no file exists.
// Unknown fields or trailing data are an error (strict decode).
func readSbxState(name string) (st sbxState, found bool, err error) {
	raw, err := os.ReadFile(sbxStatePath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return sbxState{}, true, fmt.Errorf("decoding %s: %w", sbxStatePath(name), err)
	}
	if dec.More() {
		return sbxState{}, true, fmt.Errorf("decoding %s: trailing data", sbxStatePath(name))
	}
	return st, true, nil
}

func writeSbxState(st sbxState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	p := sbxStatePath(st.Name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return pool.WriteFileAtomic(p, append(data, '\n'), 0o600)
}

// removeSbxState deletes the state file; a missing file is not an error.
func removeSbxState(name string) error {
	if err := os.Remove(sbxStatePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// sbxStateDrift names the compared fields that differ (R7: agent, template,
// mounts with modes, env names, skills mode) plus the pinhole resource: the
// sandbox's network rule keeps allowing the old port. WraptoolVersion is not
// compared.
func sbxStateDrift(have, want sbxState) []string {
	var d []string
	if have.Agent != want.Agent {
		d = append(d, "agent")
	}
	if have.Template != want.Template {
		d = append(d, "template")
	}
	if !slices.Equal(have.Mounts, want.Mounts) {
		d = append(d, "mounts")
	}
	if !slices.Equal(have.EnvNames, want.EnvNames) {
		d = append(d, "environment")
	}
	if have.Skills != want.Skills {
		d = append(d, "skills")
	}
	if have.PinholeResource != want.PinholeResource {
		d = append(d, "pinhole")
	}
	return d
}

// sbxPoolPlan is resolveSbxPoolPlan's outcome: the pool runtime/ tree (mounted
// read-only) and the only PATH dir. There is no legacy fallback (D6).
type sbxPoolPlan struct {
	runtimeDir string
	pathDirs   []string
	err        error
}

// sbxPoolPlanForHarness resolves the sbx pool plan for the named harness
// against the host pool, creating only home/ and runtime/ (never legacy bin/
// and lib/, D6). runUp calls it before the server is touched.
func sbxPoolPlanForHarness(name string) (sbxPoolPlan, error) {
	desc, ok := harness.Registry[name]
	if !ok {
		return sbxPoolPlan{}, fmt.Errorf("unknown harness %q resolving the pool plan (registry lookup after validation — this is a bug)", name)
	}
	for _, d := range []string{pool.HomeDir(), pool.RuntimeDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return sbxPoolPlan{}, fmt.Errorf("creating harness pool directory %s: %w", d, err)
		}
	}
	var pointerPath string
	if key, err := pool.HostPlatformKey(); err == nil {
		pointerPath = pool.PointerFile(key, name)
	}
	plan := resolveSbxPoolPlan(poolPlanInput{
		Harness:               name,
		PoolLockable:          desc.PoolLockable(),
		PointerPath:           pointerPath,
		RuntimeDir:            pool.RuntimeDir(),
		RuntimeBinDir:         pool.RuntimeBinDir(),
		RuntimeGenerationsDir: pool.RuntimeGenerationsDir(),
	})
	return plan, plan.err
}

// resolveSbxPoolPlan applies resolveGuixPoolPlan's liveness rule (pointer
// readable and non-blank, dispatcher present, generation binary present) but
// with no legacy fallback: every non-live outcome is an actionable error.
func resolveSbxPoolPlan(in poolPlanInput) sbxPoolPlan {
	if !in.PoolLockable {
		return sbxPoolPlan{err: fmt.Errorf("harness %q is not pool-lockable and is unsupported on --runtime=sbx; use a pool-lockable harness or another runtime", in.Harness)}
	}
	noPointer := sbxPoolPlan{err: fmt.Errorf("harness %q has no active generation in the harness pool; run `wraptool harness install %s`", in.Harness, in.Harness)}
	if in.PointerPath == "" {
		return noPointer
	}
	raw, err := pool.ReadPointer(in.PointerPath)
	if err != nil {
		return noPointer
	}
	genID := strings.TrimSpace(raw)
	if genID == "" {
		return noPointer
	}
	dispatcher := filepath.Join(in.RuntimeBinDir, in.Harness)
	genBin := filepath.Join(in.RuntimeGenerationsDir, genID, "bin", in.Harness)
	if !fileExists(dispatcher) || !fileExists(genBin) {
		return sbxPoolPlan{err: fmt.Errorf("active generation %s for harness %q is missing on disk; run `wraptool harness sync`", genID, in.Harness)}
	}
	return sbxPoolPlan{runtimeDir: in.RuntimeDir, pathDirs: []string{in.RuntimeBinDir}}
}

// buildSbxCreateArgs is the §5.1 create argv (without the leading "sbx").
func buildSbxCreateArgs(name, template, root, runtimeDir, homeDir string) []string {
	args := []string{"create", "--name", name}
	if template != "" {
		args = append(args, "--template="+template)
	}
	args = append(args, "--skills", sbxSkillsMode,
		"-e", "WRAPTOOL_CONTAINER=1",
		"-e", "WRAPTOOL_CWD="+filepath.ToSlash(root),
		sbxAgent, filepath.ToSlash(root), filepath.ToSlash(runtimeDir)+":ro", filepath.ToSlash(homeDir))
	return args
}

// buildSbxPolicyArgs is the sandbox-scoped, single-port pinhole (§5.1 step 5).
func buildSbxPolicyArgs(name string, port string) []string {
	return []string{"policy", "allow", "network", "--sandbox", name, "localhost:" + port}
}

// buildSbxPolicyCheckArgs is the R8 loopback probe for one target.
func buildSbxPolicyCheckArgs(name, target string) []string {
	return []string{"policy", "check", "network", "--sandbox", name, target}
}

// buildSbxExecArgs enters the sandbox; bash -lc runs inside it, as one argv
// element of sbx exec, never on the host.
func buildSbxExecArgs(name string, tty bool, entry string) []string {
	flag := "-i"
	if tty {
		flag = "-it"
	}
	return []string{"exec", flag, name, "bash", "-lc", entry}
}

// classifySbxProbe reads R8's output: a line starting "Allowed:" is allowed,
// "Denied:" denied, anything else inconclusive. The exit status is not used.
func classifySbxProbe(out string) (allowed, denied bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Allowed:"):
			return true, false
		case strings.HasPrefix(line, "Denied:"):
			return false, true
		}
	}
	return false, false
}

// sbxBringUpInput carries everything bringUpSbxContainer needs.
type sbxBringUpInput struct {
	Root        string
	Token       string
	Listen      string // cfg.MCP.Listen
	AdminListen string // cfg.Admin.Listen; "" = no review UI to probe
	Harness     string
	Template    string
	Shell       bool
	Exists      bool        // from the preflight's `sbx ls --quiet`
	Plan        sbxPoolPlan // resolved before the server was touched (sbxPoolPlanForHarness)
}

// sbxStdinIsTTY picks -it vs -i; a var so tests can pin both.
var sbxStdinIsTTY = func() bool { return isTTY(os.Stdin) }

// bringUpSbxContainer runs §5.1 steps 2–7. Preflight (step 1) has already run.
func bringUpSbxContainer(in sbxBringUpInput) error {
	_, port, err := net.SplitHostPort(in.Listen)
	if err != nil {
		return fmt.Errorf("parsing mcp.listen %q: %w", in.Listen, err)
	}
	plan, homeDir := in.Plan, pool.HomeDir()

	name := sbxSandboxName(in.Root)
	want := sbxState{
		Name:     name,
		Agent:    sbxAgent,
		Template: in.Template,
		Mounts: []string{
			filepath.ToSlash(in.Root),
			filepath.ToSlash(plan.runtimeDir) + ":ro",
			filepath.ToSlash(homeDir),
		},
		EnvNames:        slices.Clone(sbxCreateEnvNames),
		Skills:          sbxSkillsMode,
		PinholeResource: "localhost:" + port,
		WraptoolVersion: buildinfo.Version(),
	}

	// Drift / foreign-sandbox refusal comes before every mutating sbx call.
	if in.Exists {
		have, found, err := readSbxState(name)
		if err != nil {
			return fmt.Errorf("sandbox %s exists but its state file is unreadable (%w); inspect it, or remove the sandbox with `wraptool down --rm`", name, err)
		}
		if !found {
			return fmt.Errorf("sandbox %s exists but has no wraptool state file (%s), so it was not created by this wraptool; refusing to reuse it — remove it with `wraptool down --rm`", name, sbxStatePath(name))
		}
		if have.PinholeResource != want.PinholeResource {
			return fmt.Errorf("sandbox %s was created for %q but this run needs %q (the MCP port changed); its network rule still allows the old port and cannot be changed in place — recreate it with `wraptool down --rm` (this deletes the sandbox's VM state)", name, have.PinholeResource, want.PinholeResource)
		}
		if drift := sbxStateDrift(have, want); len(drift) > 0 {
			return fmt.Errorf("sandbox %s was created with different %s than this run wants; its mounts and environment are fixed at creation — recreate it with `wraptool down --rm` (this deletes the sandbox's VM state)", name, strings.Join(drift, ", "))
		}
	}

	base := "http://host.docker.internal:" + port
	mcpPath, err := harness.Write(in.Harness, harness.Params{
		Transport: "sse",
		URL:       base + "/sse",
		Token:     in.Token,
		CWD:       in.Root,
		Dir:       in.Root,
	})
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (wraptool @ %s)\n", mcpPath, base)

	if !in.Exists {
		fmt.Printf("creating sandbox %s ...\n", name)
		if _, errOut, err := sbxCapture(buildSbxCreateArgs(name, in.Template, in.Root, plan.runtimeDir, homeDir)); err != nil {
			return fmt.Errorf("sbx create failed: %w: %s", err, strings.TrimSpace(errOut))
		}
		if err := writeSbxState(want); err != nil {
			return fmt.Errorf("sandbox %s was created but its state file could not be written (%w); remove it with `wraptool down --rm` and retry", name, err)
		}
	}

	if _, errOut, err := sbxCapture(buildSbxPolicyArgs(name, port)); err != nil {
		return fmt.Errorf("adding the loopback pinhole localhost:%s failed: %w: %s\n"+
			"if sbx has no network policy yet, run `sbx policy init balanced` and retry", port, err, strings.TrimSpace(errOut))
	}

	probeSbxLoopback(name, in.AdminListen)

	if !in.Shell {
		fmt.Printf("\nsandbox %s is ready. Enter it with:\n  wraptool up --runtime=sbx --shell=always\n", name)
		return nil
	}
	fmt.Printf("\nopening a shell in sandbox %s (re-enter with `wraptool up --runtime=sbx`)...\n", name)
	entry := buildContainerEntry(in.Root, filepath.ToSlash(homeDir), toSlashAll(plan.pathDirs))
	return sbxAttach(buildSbxExecArgs(name, sbxStdinIsTTY(), entry))
}

func toSlashAll(dirs []string) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = filepath.ToSlash(d)
	}
	return out
}

// probeSbxLoopback is R8: after the pinhole exists, ask sbx whether the
// sandbox can also reach the review UI port and SSH on host loopback. It only
// warns; it never fails `up`.
func probeSbxLoopback(name, adminListen string) {
	var targets []string
	if adminListen != "" {
		if _, p, err := net.SplitHostPort(adminListen); err == nil && p != "" {
			targets = append(targets, "localhost:"+p)
		}
	}
	targets = append(targets, "localhost:22")
	for _, target := range targets {
		out, errOut, _ := sbxCapture(buildSbxPolicyCheckArgs(name, target))
		allowed, denied := classifySbxProbe(out + "\n" + errOut)
		switch {
		case allowed:
			fmt.Fprintf(os.Stderr, "WARNING: sandbox %s can reach host %s through the developer's global network policy; "+
				"the sandbox is not limited to the wraptool pinhole. Run `sbx policy init balanced` "+
				"(a sandbox-scoped deny cannot fix it: deny beats the pinhole's allow).\n", name, target)
		case denied:
		default:
			fmt.Fprintf(os.Stderr, "notice: could not tell whether sandbox %s can reach host %s (unrecognised `sbx policy check` output)\n", name, target)
		}
	}
}
