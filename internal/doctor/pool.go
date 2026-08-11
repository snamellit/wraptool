// pool.go implements the pool.* group (design §7.9): checks over the
// developer-scoped harness pool ($XDG_DATA_HOME/wraptool/harness-pool) that
// `wraptool harness list` does not already surface. Deliberately small — see
// the design doc for what was dropped and why (pool.orphan-generations,
// pool.lock-recipe-drift).
//
// internal/pool's zero-argument layout accessors (pool.Root, pool.HomeDir,
// ...) are BANNED here (invariants_test.go's invariantsForbiddenHostSelectors)
// because they resolve through paths.DataDir() directly, bypassing
// Input.Paths.PoolRoot. Every path below is built from PoolRoot by hand,
// mirroring internal/pool/layout.go's own Root()-relative layout.
//
// pool.generation-digest and pool.generation-corrupt additionally call
// pool.ActiveGeneration/pool.VerifyGenerationTree, the sanctioned
// root-parameterized escape hatch named in host.go's Env doc comment — both
// touch the real filesystem directly (bypassing Env) and are therefore
// tested against a t.TempDir() fixture tree, never a MapEnv (plan §1 rule 10,
// design §12).
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pti/wraptool/internal/pool"
)

const (
	poolHomePerms                 CheckID = "pool.home-perms"
	poolGenerationDigest          CheckID = "pool.generation-digest"
	poolGenerationCorrupt         CheckID = "pool.generation-corrupt"
	poolDevcontainerMountMissing  CheckID = "pool.devcontainer-mount-missing"
	poolDevcontainerMountWritable CheckID = "pool.devcontainer-mount-writable"
	poolDeprecatedFeature         CheckID = "pool.deprecated-feature"
)

// poolHomePermsMeta, poolGenerationDigestMeta, ... are the checks' Meta
// literals, named so pool_test.go's TestPoolCheckMeta can assert each check's
// ID/Needs directly against the same literal init() registers — never through
// Registered() (plan §1 rule 8: a test asserting a check's own behaviour must
// not go through the shared registry, which by the time this package's other
// eight Phase-B siblings have merged also contains every one of THEIR checks).
var (
	poolHomePermsMeta = Meta{
		ID:    poolHomePerms,
		Needs: NeedsModeBits,
		Summary: "checks the harness pool's home/ tree for group- or other-readable " +
			"credential-bearing files",
	}
	poolGenerationDigestMeta = Meta{
		ID:    poolGenerationDigest,
		Needs: NeedsNothing,
		Summary: "verifies each active harness generation's directory name against its own receipt, " +
			"distinguishing tampering from a rollback",
	}
	poolGenerationCorruptMeta = Meta{
		ID:      poolGenerationCorrupt,
		Needs:   NeedsPoolVerify,
		Summary: "verifies each active harness generation's on-disk content against its receipt (--verify-pool)",
	}
	poolDevcontainerMountMissingMeta = Meta{
		ID:      poolDevcontainerMountMissing,
		Needs:   NeedsWorkspace,
		Summary: "checks that devcontainer.json declares the harness pool runtime mount when one is needed",
	}
	poolDevcontainerMountWritableMeta = Meta{
		ID:      poolDevcontainerMountWritable,
		Needs:   NeedsWorkspace,
		Summary: "checks that a declared harness pool runtime mount is genuinely read-only",
	}
	poolDeprecatedFeatureMeta = Meta{
		ID:      poolDeprecatedFeature,
		Needs:   NeedsWorkspace,
		Summary: "checks devcontainer.json for the deprecated or sunset wraptool devcontainer feature",
	}
)

func init() {
	register(poolHomePermsMeta, runPoolHomePerms)
	register(poolGenerationDigestMeta, runPoolGenerationDigest)
	register(poolGenerationCorruptMeta, runPoolGenerationCorrupt)
	register(poolDevcontainerMountMissingMeta, runPoolDevcontainerMountMissing)
	register(poolDevcontainerMountWritableMeta, runPoolDevcontainerMountWritable)
	register(poolDeprecatedFeatureMeta, runPoolDeprecatedFeature)
}

// ---------------------------------------------------------------- pool.home-perms

// poolMaxOffendingListed bounds how many offending paths pool.home-perms
// names verbatim in Finding.Detail. A real pool's home/ tree can hold
// thousands of entries (an npm-sourced harness's skills/cache directories);
// naming every one of them produces an unreadable, single-line wall of text
// whose primary fact — HOW MANY — gets buried. The count is always reported
// in full; only the enumerated list is capped, with "and N more" naming the
// remainder.
const poolMaxOffendingListed = 5

// poolMaxWalkDepth bounds the recursive descent into home/ (relative to
// home/ itself, i.e. home/ is depth 0). Unbounded recursion is the same cost
// concern design §7.9 uses to gate pool.generation-corrupt behind
// --verify-pool, applied here to directory traversal rather than content
// hashing: a pathological or maliciously deep tree must not make every
// default `wraptool doctor` run scan without bound. Real credential state
// (dotfiles, .config/<harness>/auth*) lives well within this depth.
const poolMaxWalkDepth = 10

// poolWalkSkipDirNames are directory basenames pruned from the walk
// entirely — never descended into, never themselves checked. These hold
// large volumes of ordinary cache/dependency files, never a harness's auth
// state, so descending into them only adds cost without ever matching
// poolIsCredentialPath.
var poolWalkSkipDirNames = map[string]bool{
	"node_modules": true,
	".git":         true,
	".cache":       true,
	".npm":         true,
}

// poolIsCredentialPath reports whether relSlash — a home/-relative,
// forward-slash path — names harness AUTH STATE specifically: the
// architect-ruled scope for pool.home-perms (not every cache file under
// .npm/.cache, which the check used to flag on every pool wraptool itself
// creates via ensureHarnessPool's 0o755 os.MkdirAll — see this check's own
// doc comment). Matches:
//   - a basename of exactly ".credentials.json" (Claude's own auth file), or
//     one containing "credential", "token", or "session" (case-insensitive)
//     — opencode's auth.json is caught by the .config/*/auth* rule below,
//     not this one;
//   - any path with a ".config" segment followed, anywhere further down the
//     path, by a segment whose name starts with "auth" (case-insensitive) —
//     ".config/<harness>/auth*".
func poolIsCredentialPath(relSlash string) bool {
	segs := strings.Split(relSlash, "/")
	base := strings.ToLower(segs[len(segs)-1])
	if base == ".credentials.json" ||
		strings.Contains(base, "credential") ||
		strings.Contains(base, "token") ||
		strings.Contains(base, "session") {
		return true
	}
	for i, s := range segs {
		if !strings.EqualFold(s, ".config") {
			continue
		}
		for _, later := range segs[i+1:] {
			if strings.HasPrefix(strings.ToLower(later), "auth") {
				return true
			}
		}
	}
	return false
}

// runPoolHomePerms walks the pool's home/ tree (recursively, symlinks never
// followed or descended into — a symlink's own mode bits are meaningless on
// most hosts and would otherwise produce a near-universal false positive;
// see poolWalkSkipDirNames/poolMaxWalkDepth for the traversal's cost bound)
// looking for a group- or other-readable CREDENTIAL-BEARING file or
// directory (poolIsCredentialPath) — never the whole tree, and never home/
// itself: cmd/guixcontainer.go's ensureHarnessPool creates home/ with
// os.MkdirAll(home, 0o755), so a bare "home/ is group-readable" fail fired on
// every pool wraptool itself creates, directly contradicting
// SeverityFail's own doc comment ("never fires on a configuration working as
// designed"). Severity is warn, not fail: the remedy is a per-file chmod, not
// a recursive one — `chmod -R go-rwx` on home/ can lock a devcontainer's
// non-root user out of its own bind-mounted HOME (design §12.3's
// `WRAPTOOL_HARNESS_HOME`), so this check deliberately never recommends it.
func runPoolHomePerms(_ context.Context, in *Input) Result {
	home := filepath.Join(in.Paths.PoolRoot, "home")
	fi, err := in.Env.Stat(home)
	if err != nil {
		return Skipped("no harness pool home/ directory yet")
	}
	if !fi.IsDir() {
		return Skipped("harness pool home/ is not a directory")
	}

	var offending []FileInfo
	poolCollectReadable(in.Env, home, home, 0, &offending)

	if len(offending) == 0 {
		return Pass()
	}
	sort.Slice(offending, func(i, j int) bool { return offending[i].Path < offending[j].Path })

	listed := offending
	more := 0
	if len(listed) > poolMaxOffendingListed {
		more = len(listed) - poolMaxOffendingListed
		listed = listed[:poolMaxOffendingListed]
	}
	parts := make([]string, len(listed))
	for i, o := range listed {
		parts[i] = fmt.Sprintf("%s (mode %04o)", o.Path, o.Perm())
	}
	list := strings.Join(parts, ", ")
	if more > 0 {
		list = fmt.Sprintf("%s, and %d more", list, more)
	}

	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "Harness pool credential files are group- or other-readable",
		Detail: fmt.Sprintf(
			"%d credential-bearing path(s) under %s are group- or other-readable (%s) — these hold harness "+
				"auth state (credentials, tokens, sessions) and home/ is bind-mounted read-write into every "+
				"project container",
			len(offending), home, list),
		Remedy: "tighten permissions on the credential file(s) named above individually, e.g. `chmod go-rwx <path>` " +
			"per file — do NOT chmod -R the whole home/ tree, which can lock a devcontainer's non-root user out " +
			"of its bind-mounted HOME",
		DocRef: "design/harness/unified-harness-pool.md",
	})
}

// poolCollectReadable appends every group- or other-readable CREDENTIAL-
// BEARING file/directory (poolIsCredentialPath, relative to home) under dir
// (recursively, best-effort: an unreadable subdirectory is skipped rather
// than failing the whole walk, and poolWalkSkipDirNames/poolMaxWalkDepth
// bound the traversal's cost) to out. Symlinked entries are neither checked
// nor descended into.
func poolCollectReadable(env Env, home, dir string, depth int, out *[]FileInfo) {
	if depth > poolMaxWalkDepth {
		return
	}
	entries, err := env.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsSymlink() {
			continue
		}
		if e.IsDir() && poolWalkSkipDirNames[strings.ToLower(e.Name)] {
			continue
		}
		rel, relErr := filepath.Rel(home, e.Path)
		if relErr != nil {
			rel = e.Path
		}
		if e.GroupOrOtherReadable() && poolIsCredentialPath(filepath.ToSlash(rel)) {
			*out = append(*out, e)
		}
		if e.IsDir() {
			poolCollectReadable(env, home, e.Path, depth+1, out)
		}
	}
}

// ---------------------------------------------------------------- pool.generation-digest / pool.generation-corrupt

// poolActiveEntry is one (harness, platform) pair that currently has a live
// active pointer, paired with the digest harness-lock.json records for it.
type poolActiveEntry struct {
	Harness        string
	Platform       pool.PlatformKey
	GenerationID   string
	RecordedDigest string
}

// poolActiveGenerationEntries reads harness-lock.json (via Env — a plain
// data file, not one of pool's zero-arg layout accessors) and, for every
// (harness, platform) it records a generation digest for, resolves the
// CURRENTLY ACTIVE generation id via pool.ActiveGeneration (the sanctioned
// root-parameterized, host-touching exception). A platform with no active
// pointer is silently omitted — this is not itself a finding (that is what
// pool.devcontainer-mount-missing's "an active pointer" gate and `harness
// list`'s REALIZED/ACTIVE columns are for).
//
// ok is false when the lock could not be read or parsed at all, in which
// case skip is the Result callers should return verbatim.
func poolActiveGenerationEntries(in *Input) (entries []poolActiveEntry, pp pool.PoolPaths, skip Result, ok bool) {
	pp = pool.PoolPaths{Runtime: filepath.Join(in.Paths.PoolRoot, "runtime")}

	lockPath := filepath.Join(in.Paths.PoolRoot, "harness-lock.json")
	data, err := in.Env.ReadFile(lockPath)
	if err != nil {
		return nil, pp, Skipped("no harness pool lock (harness-lock.json) yet"), false
	}
	lock, err := pool.UnmarshalLock(data)
	if err != nil {
		return nil, pp, Skipped("harness-lock.json could not be parsed: %v", err), false
	}

	for _, h := range poolSortedHarnessNames(lock.Harnesses) {
		lh := lock.Harnesses[h]
		for _, key := range poolSortedPlatformKeys(lh.Generations) {
			genID, active := pool.ActiveGeneration(pp, h, key)
			if !active {
				continue
			}
			entries = append(entries, poolActiveEntry{
				Harness:        h,
				Platform:       key,
				GenerationID:   genID,
				RecordedDigest: lh.Generations[key],
			})
		}
	}
	return entries, pp, Result{}, true
}

func poolSortedHarnessNames(m map[string]pool.LockHarness) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func poolSortedPlatformKeys(m map[pool.PlatformKey]string) []pool.PlatformKey {
	out := make([]pool.PlatformKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// runPoolGenerationDigest is the cheap, always-on tamper check (design §7.9).
// The receipt lives inside the tree it attests, so a naive tree-vs-receipt
// comparison alone cannot catch an attacker who rewrites both; the
// discriminator this check actually uses is the active generation's own
// DIRECTORY NAME, which encodes the digest its receipt had at REALIZE time
// (`pool.GenerationID`, `internal/pool/receipt.go`): recomputing
// GenerationID(receipt.Harness, receipt.Version, platform,
// receipt.ManifestDigest) and comparing it against the pointed-to directory
// tells apart two very different conditions that a plain "does the receipt's
// digest match harness-lock.json's recorded one" comparison conflates:
//
//   - the recomputed id does NOT match the directory name: receipt.json was
//     rewritten in place without a matching rename — genuinely tampered, a
//     fail. (An attacker who also renames the directory to match still
//     cannot forge harness-lock.json's independent audit trail — that is a
//     separate, unaddressed threat this check does not claim to cover.)
//   - the recomputed id DOES match the directory name (this generation's
//     content is exactly what its own receipt says) but that digest differs
//     from harness-lock.json's CURRENTLY recorded one for this platform: an
//     EARLIER generation is active — i.e. a legitimate
//     `wraptool harness rollback` — reported as info, not a fail whose
//     remedy would silently undo the rollback.
//
// A generation whose receipt.json is missing or unparseable is silently
// skipped here (not this check's concern — mirrors internal/pool/gc.go's
// scanGenerationRecords, which excludes rather than errors on the same
// condition); it is exactly the class pool.generation-corrupt's fuller
// content scan is for.
func runPoolGenerationDigest(_ context.Context, in *Input) Result {
	entries, pp, skip, ok := poolActiveGenerationEntries(in)
	if !ok {
		return skip
	}

	var findings []Finding
	for _, e := range entries {
		receiptPath := filepath.Join(pp.Runtime, "generations", e.GenerationID, "receipt.json")
		data, err := in.Env.ReadFile(receiptPath)
		if err != nil {
			continue
		}
		receipt, err := pool.UnmarshalReceipt(data)
		if err != nil {
			continue
		}

		expectedID, idErr := pool.GenerationID(receipt.Harness, receipt.Version, e.Platform, receipt.ManifestDigest)
		switch {
		case idErr != nil || expectedID != e.GenerationID:
			detail := fmt.Sprintf(
				"%s (%s): active generation directory %s does not match the generation id its own receipt.json "+
					"would produce (harness=%s version=%s digest=%s) — the receipt may have been rewritten "+
					"without a matching rename",
				e.Harness, e.Platform, e.GenerationID, receipt.Harness, receipt.Version, receipt.ManifestDigest)
			if idErr != nil {
				detail += fmt.Sprintf(" (recomputing the generation id failed: %v)", idErr)
			}
			findings = append(findings, Finding{
				Severity: SeverityFail,
				Title:    "Active harness generation does not match its own receipt",
				Detail:   detail,
				Location: receiptPath,
				Remedy: fmt.Sprintf(
					"run `wraptool harness sync --platform %s` to rebuild %s, or investigate before trusting this generation",
					e.Platform, e.Harness),
			})
		case receipt.ManifestDigest != e.RecordedDigest:
			findings = append(findings, Finding{
				Severity: SeverityInfo,
				Title:    "An earlier harness generation is active",
				Detail: fmt.Sprintf(
					"%s (%s): active generation %s's digest %s differs from harness-lock.json's currently "+
						"recorded digest %s for this platform, but the generation's own content matches its "+
						"receipt — this looks like a deliberate `wraptool harness rollback`, not tampering",
					e.Harness, e.Platform, e.GenerationID, receipt.ManifestDigest, e.RecordedDigest),
				Location: receiptPath,
			})
		}
	}
	return Found(findings...)
}

// runPoolGenerationCorrupt is the opt-in (--verify-pool) full content scan:
// pool.VerifyGenerationTree sha256-hashes every file in an active generation's
// tree, which is 10k+ files and hundreds of MB for a node harness — far too
// slow for a default run (design §7.9), hence NeedsPoolVerify.
//
// doctor never takes the pool lock (design §7.9's own header note), so a
// concurrent `wraptool harness gc` can remove or restrict this generation's
// tree mid-scan; ScanGenerationFiles/VerifyGenerationTree surface that as a
// plain I/O error (fs.ErrNotExist/fs.ErrPermission), which is a RACE, not
// corruption, and is silently skipped rather than reported as a fail whose
// remedy would tell the operator to rebuild a perfectly healthy pool.
func runPoolGenerationCorrupt(_ context.Context, in *Input) Result {
	entries, pp, skip, ok := poolActiveGenerationEntries(in)
	if !ok {
		return skip
	}

	var findings []Finding
	for _, e := range entries {
		genDir := filepath.Join(pp.Runtime, "generations", e.GenerationID)
		data, err := in.Env.ReadFile(filepath.Join(genDir, "receipt.json"))
		if err != nil {
			continue
		}
		receipt, err := pool.UnmarshalReceipt(data)
		if err != nil {
			continue
		}
		if err := pool.VerifyGenerationTree(genDir, receipt); err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				continue
			}
			findings = append(findings, Finding{
				Severity: SeverityFail,
				Title:    "Active harness generation tree does not match its receipt",
				Detail: fmt.Sprintf("%s (%s): generation %s failed content verification: %v",
					e.Harness, e.Platform, e.GenerationID, err),
				Location: genDir,
				Remedy: fmt.Sprintf(
					"run `wraptool harness sync --platform %s` to rebuild %s, or investigate before trusting this generation",
					e.Platform, e.Harness),
			})
		}
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- devcontainer mount + feature checks

// poolDevcontainerRuntimeTarget is the container-side mount point the guix
// AND devcontainer runtimes both expect the pool's runtime/ tree at
// (cmd/guixcontainer.go's devcontainerPoolRuntime const, features/src/wraptool's
// containerEnv PATH). internal/doctor cannot import cmd (cmd imports this
// package), so the value is duplicated rather than shared — keep it in sync
// with cmd/guixcontainer.go if that ever changes.
const poolDevcontainerRuntimeTarget = "/opt/wraptool-harness/runtime"

// poolCurrentFeatureRef is the wiring-only feature's current major
// (cmd/init_devcontainer.go's wraptoolFeatureRef), used only to word
// pool.deprecated-feature's remedy text.
const poolCurrentFeatureRef = "forge.snamellit.com/pti/wraptool/wraptool:1"

func poolDevcontainerConfigPaths(workspace string) []string {
	return []string{
		filepath.Join(workspace, ".devcontainer", "devcontainer.json"),
		filepath.Join(workspace, ".devcontainer.json"),
	}
}

// poolDevcontainerFile pairs one devcontainer config file's content with the
// path it was read from — both .devcontainer/devcontainer.json and
// .devcontainer.json are accepted (poolDevcontainerConfigPaths), and every
// finding below carries WHICH one it actually read into Finding.Location
// rather than leaving the operator to guess.
type poolDevcontainerFile struct {
	Path string
	Data []byte
}

// poolReadDevcontainerFiles reads every existing devcontainer config file in
// workspace via Env (never os directly). found is false when neither exists,
// which every caller here treats as Skipped — the devcontainer runtime does
// not apply to this workspace at all.
func poolReadDevcontainerFiles(env Env, workspace string) (files []poolDevcontainerFile, found bool) {
	for _, p := range poolDevcontainerConfigPaths(workspace) {
		data, err := env.ReadFile(p)
		if err != nil {
			continue
		}
		files = append(files, poolDevcontainerFile{Path: p, Data: data})
	}
	return files, len(files) > 0
}

// poolFilePaths returns the sorted, comma-joined paths of files — used as a
// finding's Location when a check is reporting on an ABSENCE across every
// devcontainer config file it consulted, rather than a specific offending
// entry in one of them.
func poolFilePaths(files []poolDevcontainerFile) string {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	sort.Strings(paths)
	return strings.Join(paths, ", ")
}

// poolDevcontainerDoc is the subset of devcontainer.json this file cares
// about: the mounts array (pool.devcontainer-mount-*) and the features map
// (pool.deprecated-feature).
type poolDevcontainerDoc struct {
	Mounts   []any                      `json:"mounts"`
	Features map[string]json.RawMessage `json:"features"`
}

func poolParseDevcontainerDoc(data []byte) (poolDevcontainerDoc, bool) {
	var doc poolDevcontainerDoc
	if err := json.Unmarshal([]byte(poolStripJSONC(string(data))), &doc); err != nil {
		return poolDevcontainerDoc{}, false
	}
	return doc, true
}

// poolTrailingCommaRE removes a JSON-illegal trailing comma before a closing
// } or ] — JSONC/JSON5 permit them. Mirrors cmd/up.go's
// devcontainerTrailingCommaRE (see that var's doc comment for the accepted
// string-unaware residual risk, which applies here identically).
var poolTrailingCommaRE = regexp.MustCompile(`,(\s*[}\]])`)

// poolStripJSONC removes `//` line comments and `/* */` block comments from
// raw JSONC text, tracking JSON string boundaries so a comment marker inside
// a string is left alone, then removes trailing commas. Mirrors cmd/up.go's
// stripJSONC; duplicated rather than shared because internal/doctor cannot
// import cmd (see poolDevcontainerRuntimeTarget's doc comment).
func poolStripJSONC(data string) string {
	var b strings.Builder
	b.Grow(len(data))
	inString := false
	i, n := 0, len(data)
	for i < n {
		c := data[i]
		if inString {
			b.WriteByte(c)
			if c == '\\' && i+1 < n {
				b.WriteByte(data[i+1])
				i += 2
				continue
			}
			if c == '"' {
				inString = false
			}
			i++
			continue
		}
		switch {
		case c == '"':
			inString = true
			b.WriteByte(c)
			i++
		case c == '/' && i+1 < n && data[i+1] == '/':
			for i < n && data[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && data[i+1] == '*':
			i += 2
			for i+1 < n && (data[i] != '*' || data[i+1] != '/') {
				i++
			}
			if i+1 < n {
				i += 2
			} else {
				i = n
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return poolTrailingCommaRE.ReplaceAllString(b.String(), "$1")
}

// poolParseMountString parses Docker's comma-separated mount string form
// ("source=...,target=...,type=bind,readonly") into a key=value field map AND
// a set of bare boolean flag tokens (readonly/ro have no "="). Mirrors
// cmd/up.go's devcontainerParseMountString.
func poolParseMountString(s string) (fields map[string]string, flags map[string]bool) {
	fields = map[string]string{}
	flags = map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok {
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		} else {
			flags[part] = true
		}
	}
	return fields, flags
}

// poolMountState classifies what a devcontainer.json mounts-array entry at
// some container target actually achieves. Closed enum (exhaustive lint):
// see cmd/up.go's devcontainerMountReadOnlyState, which this mirrors —
// object-form mounts have no read-only field at all (the CLI silently drops
// one if present), so only a STRING-form, type=bind entry carrying a bare
// readonly/ro token counts as genuinely read-only.
type poolMountState int

const (
	poolMountAbsent poolMountState = iota
	poolMountReadOnly
	poolMountReadWrite
)

// poolClassifyMount finds the first mounts-array entry (across every parsed
// devcontainer config file) targeting containerTarget and classifies it,
// along with the path of the file that entry was found in ("" when absent —
// callers reporting an absence use poolFilePaths over every file consulted
// instead, since there is no single offending file to name).
func poolClassifyMount(files []poolDevcontainerFile, containerTarget string) (state poolMountState, path string) {
	for _, f := range files {
		doc, ok := poolParseDevcontainerDoc(f.Data)
		if !ok {
			continue
		}
		for _, m := range doc.Mounts {
			switch v := m.(type) {
			case string:
				fields, flags := poolParseMountString(v)
				if fields["target"] != containerTarget {
					continue
				}
				if fields["type"] == "bind" && (flags["readonly"] || flags["ro"]) {
					return poolMountReadOnly, f.Path
				}
				return poolMountReadWrite, f.Path
			case map[string]any:
				if t, _ := v["target"].(string); t == containerTarget {
					return poolMountReadWrite, f.Path
				}
			}
		}
	}
	return poolMountAbsent, ""
}

// poolRuntimeMountSnippet is the exact `mounts` string entry to recommend:
// read-only, source = this host's real, ToSlash'd pool runtime path. Mirrors
// cmd/up.go's devcontainerRuntimeMountSnippet, INCLUDING its unsafe-character
// guard: a source containing ',', '"', or '\' cannot be embedded verbatim in
// this illustrative, human-pasted mounts-string form without corrupting it
// (a comma misparses as the CSV field separator; a quote or backslash needs
// JSON escaping this snippet doesn't do), so refuse to print a broken one and
// name the exact escaping problem instead (via strconv.Quote, which shows the
// correctly escaped form). Vanishingly rare in practice — it needs a pool
// root path containing one of these characters on a POSIX host.
func poolRuntimeMountSnippet(runtimeDir string) string {
	src := filepath.ToSlash(runtimeDir)
	if strings.ContainsAny(src, `,"\`) {
		return fmt.Sprintf("<cannot render a mounts snippet: pool runtime path %s needs manual escaping (contains "+
			"a comma, quote, or backslash) — add it to \"mounts\" by hand>", strconv.Quote(src))
	}
	return "source=" + src + ",target=" + poolDevcontainerRuntimeTarget + ",type=bind,readonly"
}

// poolAnyActivePointer reports whether ANY harness has an active generation
// pointer for ANY platform — the gate for "the operator has opted into the
// pool-locked runtime, so the shared runtime/ mount is expected". A best-
// effort presence check (a *.gen file exists under runtime/active/<key>/),
// not a validity check: pool.generation-digest/-corrupt own content
// validity, this only decides whether the mount checks apply at all.
func poolAnyActivePointer(env Env, poolRoot string) bool {
	activeDir := filepath.Join(poolRoot, "runtime", "active")
	platforms, err := env.ReadDir(activeDir)
	if err != nil {
		return false
	}
	for _, p := range platforms {
		if !p.IsDir() {
			continue
		}
		entries, err := env.ReadDir(p.Path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name, ".gen") {
				return true
			}
		}
	}
	return false
}

// runPoolDevcontainerMountMissing fires when the pool is live (an active
// pointer exists) but the workspace's devcontainer.json declares no mount at
// the runtime target at all. Mirrors cmd/up.go's own verify-only notice
// (`wraptool up` never injects this mount itself, §12.2).
//
// No active pointer at all means there is no pool-locked runtime for the
// mount to carry — Skipped, not Pass: §6 reserves Pass ("checked, clean") for
// a check that actually looked at its subject and found nothing wrong, and
// there is no devcontainer posture to check when there is no pool.
func runPoolDevcontainerMountMissing(_ context.Context, in *Input) Result {
	if !poolAnyActivePointer(in.Env, in.Paths.PoolRoot) {
		return Skipped("no active harness pool generation for any platform")
	}
	files, found := poolReadDevcontainerFiles(in.Env, in.Workspace)
	if !found {
		return Skipped("no .devcontainer/devcontainer.json in this workspace")
	}
	if state, _ := poolClassifyMount(files, poolDevcontainerRuntimeTarget); state != poolMountAbsent {
		return Pass()
	}

	runtimeDir := filepath.Join(in.Paths.PoolRoot, "runtime")
	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "Harness pool runtime mount is not declared",
		Detail: fmt.Sprintf(
			"a harness has an active pool-locked generation, but devcontainer.json declares no mount at %s",
			poolDevcontainerRuntimeTarget),
		Location: poolFilePaths(files),
		Remedy:   fmt.Sprintf("add this entry to \"mounts\": %q", poolRuntimeMountSnippet(runtimeDir)),
		DocRef:   "docs/devcontainers.qmd",
	})
}

// runPoolDevcontainerMountWritable fires when the runtime mount IS declared
// but not provably read-only (object-form, a string-form entry missing the
// readonly/ro token, or a non-bind type). A read-write mount of the locked
// runtime/ tree would let one project's container rewrite the dispatcher
// scripts every OTHER project's container executes (§5) — the same case
// cmd/up.go's own notice begins "SECURITY:" for.
//
// See runPoolDevcontainerMountMissing's doc comment for why no active
// pointer is Skipped rather than Pass.
func runPoolDevcontainerMountWritable(_ context.Context, in *Input) Result {
	if !poolAnyActivePointer(in.Env, in.Paths.PoolRoot) {
		return Skipped("no active harness pool generation for any platform")
	}
	files, found := poolReadDevcontainerFiles(in.Env, in.Workspace)
	if !found {
		return Skipped("no .devcontainer/devcontainer.json in this workspace")
	}
	state, path := poolClassifyMount(files, poolDevcontainerRuntimeTarget)
	if state != poolMountReadWrite {
		return Pass()
	}

	runtimeDir := filepath.Join(in.Paths.PoolRoot, "runtime")
	return Found(Finding{
		Severity: SeverityFail,
		Title:    "SECURITY: harness pool runtime mount is not read-only",
		Detail: fmt.Sprintf(
			"devcontainer.json declares a mount at %s that the devcontainer CLI will NOT enforce as read-only "+
				"(object-form mounts and non-bind types cannot express read-only through that CLI) — the locked "+
				"harness pool would be writable from inside this project's container, letting it rewrite the "+
				"dispatcher scripts every OTHER project's container executes",
			poolDevcontainerRuntimeTarget),
		Location: path,
		Remedy:   fmt.Sprintf("replace it with this read-only string-form entry in \"mounts\": %q", poolRuntimeMountSnippet(runtimeDir)),
		DocRef:   "design/harness/unified-harness-pool.md",
	})
}

// poolLastFeatureSegment returns the final "/"-separated path segment of a
// features-map key (an OCI ref, a local relative path, or a bare id).
func poolLastFeatureSegment(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// poolFeatureIDAndTag splits a features-map key's final segment into its
// feature id and version tag ("wraptool:0" -> "wraptool", "0"). A key with no
// ":" (a bare id, or a local relative path with none) yields an empty tag.
func poolFeatureIDAndTag(ref string) (id, tag string) {
	seg := poolLastFeatureSegment(ref)
	id, tag, _ = strings.Cut(seg, ":")
	return id, tag
}

// poolFeatureDeprecation reports whether ref names the deprecated `wraptool:0`
// per-image-provisioning major or the sunset `wraptool-connect` feature
// (design §12.3, CLAUDE.md's Testing section), and why.
func poolFeatureDeprecation(ref string) (reason string, deprecated bool) {
	id, tag := poolFeatureIDAndTag(ref)
	switch {
	case id == "wraptool-connect":
		return "the sunset wraptool-connect feature (no longer published; its registry artifacts still work, " +
			"but new setups should use the wraptool feature instead)", true
	case id == "wraptool" && (tag == "0" || strings.HasPrefix(tag, "0.")):
		return "the deprecated wraptool:0 per-image-provisioning feature major (pin wraptool:1, the wiring-only major, instead)", true
	default:
		return "", false
	}
}

// poolDeprecatedRef is one deprecated/sunset features-map key found in one
// devcontainer config file, paired with the file it was found in.
type poolDeprecatedRef struct {
	Ref  string
	Path string
}

// poolDeprecatedFeatureRefs returns every deprecated/sunset features-map key
// found across files, sorted by Ref and deduplicated (first file found in
// wins the Path, when the same ref appears in more than one file — a
// deliberately unlikely edge case since a workspace normally has only one
// devcontainer config file).
func poolDeprecatedFeatureRefs(files []poolDevcontainerFile) []poolDeprecatedRef {
	seen := map[string]bool{}
	var out []poolDeprecatedRef
	for _, f := range files {
		doc, ok := poolParseDevcontainerDoc(f.Data)
		if !ok {
			continue
		}
		var refs []string
		for ref := range doc.Features {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		for _, ref := range refs {
			if _, bad := poolFeatureDeprecation(ref); bad && !seen[ref] {
				seen[ref] = true
				out = append(out, poolDeprecatedRef{Ref: ref, Path: f.Path})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// runPoolDeprecatedFeature fires once per deprecated/sunset features-map key
// found in the workspace's devcontainer config.
func runPoolDeprecatedFeature(_ context.Context, in *Input) Result {
	files, found := poolReadDevcontainerFiles(in.Env, in.Workspace)
	if !found {
		return Skipped("no .devcontainer/devcontainer.json in this workspace")
	}

	refs := poolDeprecatedFeatureRefs(files)
	if len(refs) == 0 {
		return Pass()
	}

	findings := make([]Finding, 0, len(refs))
	for _, r := range refs {
		reason, _ := poolFeatureDeprecation(r.Ref)
		findings = append(findings, Finding{
			Severity: SeverityWarn,
			Title:    "devcontainer.json pins a deprecated wraptool feature",
			Detail:   fmt.Sprintf("features key %q pins %s", r.Ref, reason),
			Location: r.Path,
			Remedy:   fmt.Sprintf("update the feature reference to %q (see design/harness/unified-harness-pool.md §12.3)", poolCurrentFeatureRef),
			DocRef:   "docs/devcontainers.qmd",
		})
	}
	return Found(findings...)
}
