package doctor

import (
	"context"
	"strings"
)

const envContainerBackend CheckID = "env.container-backend"

func init() {
	register(Meta{
		ID:      envContainerBackend,
		Needs:   NeedsNothing, // no inputs at all — this is why doctor works before any config exists
		Summary: "reports which container backends are on PATH",
	}, runEnvContainerBackend)
}

// envBackends is a fixed slice, never a map: map range order is randomized per
// iteration and TestChecksAreDeterministic runs every check twice and diffs.
// Order matches cmd/guixcontainer.go's and cmd/up.go's preference.
var envBackends = []string{"guix", "docker", "podman"}

// runEnvContainerBackend is a NAMED package-level function, not a closure inside
// init(): its test calls it directly with a hand-built *Input and never touches
// the registry, which is what stops one author's tests breaking when another
// merges.
func runEnvContainerBackend(_ context.Context, in *Input) Result {
	var present, absent []string
	for _, b := range envBackends {
		if p, err := in.Env.LookPath(b); err == nil {
			present = append(present, b+" ("+p+")")
		} else {
			absent = append(absent, b)
		}
	}
	f := Finding{
		// ID is stamped by the engine from Meta.ID.
		Severity: SeverityInfo,
		Title:    "Container backends",
		Detail:   describeBackends(present, absent),
		DocRef:   "docs/isolated-environments.md",
	}
	if len(present) == 0 {
		// info, never warn (§7.1): a host-only deployment serving MCP to a
		// separately managed container is fully supported and must not be
		// nagged. env.no-backend-with-project (B1) is the check that judges,
		// and only once --workspace shows the operator opted into `up`.
		f.Remedy = "install guix, docker, or podman if you want `wraptool up` to build an isolated environment"
	}
	return Found(f)
}

func describeBackends(present, absent []string) string {
	var b strings.Builder
	if len(present) > 0 {
		b.WriteString("found: ")
		b.WriteString(strings.Join(present, ", "))
	} else {
		b.WriteString("none found on PATH")
	}
	if len(absent) > 0 {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString("not found: ")
		b.WriteString(strings.Join(absent, ", "))
	}
	return b.String()
}
