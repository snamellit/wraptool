// exposure_listen.go implements the listener/bind half of the exposure.*
// group (design §7.3): whether the MCP and admin listeners are reachable from
// somewhere they should not be. exposure_files.go (B5, its own parallel
// step) owns the token/socket/request-log half of the same design section —
// deliberately split, per plan §1's one-file-per-group rule, so the two never
// share a helper (rule 7's own worked example).
package doctor

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/pti/wraptool/internal/config"
)

const (
	exposureListenTransportUnsupportedID    CheckID = "exposure.transport-unsupported"
	exposureListenSSENonLoopbackID          CheckID = "exposure.sse-non-loopback"
	exposureListenAdminNonLoopbackID        CheckID = "exposure.admin-non-loopback"
	exposureListenAdminContainerReachableID CheckID = "exposure.admin-container-reachable"
	exposureListenListenersCollideID        CheckID = "exposure.listeners-collide"
)

// exposureListenTransportUnsupportedMeta, ... are the checks' Meta literals,
// named so a local test can assert each check's ID/Needs directly against
// the same literal init() registers — never through Registered() (plan §1
// rule 8; mirrors pool.go's poolHomePermsMeta and friends).
var (
	exposureListenTransportUnsupportedMeta = Meta{
		ID:      exposureListenTransportUnsupportedID,
		Needs:   NeedsConfig,
		Summary: "warns when mcp.transport is not one of the deployable transports",
	}
	exposureListenSSENonLoopbackMeta = Meta{
		ID:      exposureListenSSENonLoopbackID,
		Needs:   NeedsConfig,
		Summary: "warns when the (authenticated) MCP listener binds off-loopback",
	}
	exposureListenAdminNonLoopbackMeta = Meta{
		ID:      exposureListenAdminNonLoopbackID,
		Needs:   NeedsConfig,
		Summary: "fails when the admin review UI listener binds off-loopback",
	}
	exposureListenAdminContainerReachableMeta = Meta{
		ID:    exposureListenAdminContainerReachableID,
		Needs: NeedsConfig,
		Summary: "warns when the admin listener is set on a host where guix can share its network " +
			"namespace with a project container",
	}
	exposureListenListenersCollideMeta = Meta{
		ID:      exposureListenListenersCollideID,
		Needs:   NeedsConfig,
		Summary: "fails when mcp.listen and admin.listen would collide at bind time",
	}
)

func init() {
	register(exposureListenTransportUnsupportedMeta, runExposureListenTransportUnsupported)
	register(exposureListenSSENonLoopbackMeta, runExposureListenSSENonLoopback)
	register(exposureListenAdminNonLoopbackMeta, runExposureListenAdminNonLoopback)
	register(exposureListenAdminContainerReachableMeta, runExposureListenAdminContainerReachable)
	register(exposureListenListenersCollideMeta, runExposureListenListenersCollide)
}

// ------------------------------------------------------ exposure.transport-unsupported

// exposureListenSupportedTransports is the set exposure.transport-unsupported
// keys on (design §7.3). It is deliberately the ALLOW set, not a deny list of
// specific unsupported values: CLAUDE.md forbids naming wraptool's own
// test-only transport in user-facing text (docs, examples, help text,
// scaffolded defaults), and this check's finding text is exactly that
// surface — so neither the map's keys nor the Detail/Remedy/Summary/ID below
// ever spell that transport out, whether or not an operator's own config
// happens to have set it. A map, not a slice: only ever indexed by one key at
// a time, never ranged, so it carries no TestChecksAreDeterministic risk.
var exposureListenSupportedTransports = map[string]bool{
	"sse":  true,
	"unix": true,
}

// runExposureListenTransportUnsupported fires when mcp.transport is not in
// exposureListenSupportedTransports. The Detail deliberately never echoes the
// actual configured value back — see exposureListenSupportedTransports' doc
// comment — so this finding's text is identical regardless of what the
// operator actually wrote.
func runExposureListenTransportUnsupported(_ context.Context, in *Input) Result {
	if exposureListenSupportedTransports[in.Config.MCP.Transport] {
		return Pass()
	}
	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "mcp.transport is not a deployable transport",
		Detail: "mcp.transport is set to a value that is not one of the transports wraptool documents for " +
			"deployment (sse, unix)",
		Remedy: "set mcp.transport to sse (the default) or unix",
		DocRef: "docs/configuration.qmd#mcp",
	})
}

// ------------------------------------------------------ exposure.sse-non-loopback

// runExposureListenSSENonLoopback fires only for the AUTHENTICATED
// non-loopback sse case (design §7.3): Config.Validate already fails the
// unauthenticated one (internal/config/validate.go), so warning here too
// would duplicate that fail with a second, redundant finding for the same
// underlying cause. Gated on transport == "sse" specifically, never a bare
// config.ListenIsLoopback(cfg.MCP.Listen) call: that function returns false
// for a unix socket PATH (it has no host:port to parse), which would
// otherwise fire this warn on every unix-transport deployment.
func runExposureListenSSENonLoopback(_ context.Context, in *Input) Result {
	cfg := in.Config
	if cfg.MCP.Transport != "sse" || cfg.MCP.Listen == "" || cfg.MCP.AuthTokenFile == "" {
		return Pass()
	}
	if config.ListenIsLoopback(cfg.MCP.Listen) {
		return Pass()
	}
	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "MCP listener is not loopback-only",
		Detail: fmt.Sprintf(
			"mcp.transport is sse and mcp.listen %q is not loopback-only — the MCP endpoint is reachable from "+
				"other hosts on the network. An auth_token_file is configured, which is what keeps this from "+
				"being an outright Config.Validate failure", cfg.MCP.Listen),
		Remedy: "bind mcp.listen to 127.0.0.1 (or a unix socket) unless off-host reachability is genuinely intended",
		DocRef: "docs/configuration.qmd#mcp",
	})
}

// ------------------------------------------------------ exposure.admin-non-loopback

// runExposureListenAdminNonLoopback fails when the admin review UI listens
// off-loopback: it is the approval authority for capability requests, so
// wraptool's whole request-review model assumes only the local operator can
// reach it — unlike mcp.listen, there is no authenticated-and-remote case
// this check tolerates.
func runExposureListenAdminNonLoopback(_ context.Context, in *Input) Result {
	cfg := in.Config
	if cfg.Admin.Listen == "" {
		return Pass() // admin UI disabled
	}
	if config.ListenIsLoopback(cfg.Admin.Listen) {
		return Pass()
	}
	return Found(Finding{
		Severity: SeverityFail,
		Title:    "Admin review UI is not loopback-only",
		Detail: fmt.Sprintf(
			"admin.listen %q is not loopback-only — the admin UI is the sole approval authority for capability "+
				"requests and rule edits, and must not be reachable off-host", cfg.Admin.Listen),
		Remedy: "bind admin.listen to 127.0.0.1 (localhost only)",
		DocRef: "docs/configuration.qmd#admin",
	})
}

// ------------------------------------------------------ exposure.admin-container-reachable

// runExposureListenAdminContainerReachable warns when the admin UI is
// enabled on a host where `guix shell --container --network` (cmd/guixcontainer.go)
// would share the host's network namespace with a project container: a
// loopback-only admin.listen is normally safe from anything off-host, but a
// shared netns makes an in-container harness process count as "localhost"
// too, defeating that isolation. Gated on the BACKEND (guix on PATH), not on
// --workspace: the risk is a property of this server config on this host, not
// of the directory doctor happens to be run from.
func runExposureListenAdminContainerReachable(_ context.Context, in *Input) Result {
	if in.Config.Admin.Listen == "" {
		return Pass()
	}
	if _, err := in.Env.LookPath("guix"); err != nil {
		return Pass()
	}
	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "Admin UI may be reachable from inside a project container",
		Detail: fmt.Sprintf(
			"admin.listen %q is set and guix is on PATH. guix shell --container --network shares the host's "+
				"network namespace, so an in-container harness can reach the admin UI at its loopback address "+
				"just as this host can, even though admin.listen is otherwise loopback-only", in.Config.Admin.Listen),
		Remedy: "this is a property of guix's shared-netns container network mode — make sure you trust every " +
			"harness given access to a `wraptool up` container on this host, since it can reach the approval UI",
		DocRef: "docs/isolated-environments.md",
	})
}

// ------------------------------------------------------ exposure.listeners-collide

// exposureListenAddr is one listener's parsed host/port, or ok == false when
// the listen value has no host:port to parse at all (a unix socket path, or
// any other non-TCP form) — such a listener cannot collide with anything a
// port comparison could detect.
type exposureListenAddr struct {
	Host string
	Port string
}

// exposureListenParseAddr reuses net.SplitHostPort — the same parser
// internal/config's own config.ListenIsLoopback is built on (internal/config/listen.go)
// — rather than comparing the raw listen strings, which is exactly what lets
// "0.0.0.0:8717" and "127.0.0.1:8717" compare unequal as strings while still
// colliding at bind.
func exposureListenParseAddr(listen string) (exposureListenAddr, bool) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return exposureListenAddr{}, false
	}
	return exposureListenAddr{Host: host, Port: port}, true
}

// exposureListenIsWildcard reports whether host binds every interface —
// including whatever address the OTHER listener in the collision check binds.
func exposureListenIsWildcard(host string) bool {
	switch host {
	case "", "0.0.0.0", "::":
		return true
	}
	return false
}

// exposureListenBindSet is what one listener's host actually binds to, per
// the RULING in design §7.3: comparing bind SETS, not a loopback predicate.
// A loopback predicate collapses every loopback address into one boolean, so
// e.g. 127.0.0.1 and 127.0.0.2 (two distinct, non-colliding loopback IPs)
// were indistinguishable from an actual collision. Wildcard is its own case
// (overlaps everything), "localhost" expands to the two addresses it
// actually resolves to at bind time, a parseable host is that one IP, and
// anything else (a hostname doctor cannot and should not resolve — no DNS
// lookups in a static check) is compared as a literal, case-insensitive
// string against another hostname only.
type exposureListenBindSet struct {
	Wildcard bool
	IPs      []net.IP
	Hostname string // set only when Wildcard is false and IPs is empty
}

// exposureListenExpandHost builds host's exposureListenBindSet.
func exposureListenExpandHost(host string) exposureListenBindSet {
	if exposureListenIsWildcard(host) {
		return exposureListenBindSet{Wildcard: true}
	}
	if host == "localhost" {
		return exposureListenBindSet{IPs: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}}
	}
	if ip := net.ParseIP(host); ip != nil {
		return exposureListenBindSet{IPs: []net.IP{ip}}
	}
	return exposureListenBindSet{Hostname: host}
}

// exposureListenOverlap reports whether a and b — both already known to share
// a port (the caller's job) — bind overlapping host sets: a collision only
// on a NON-EMPTY intersection of what each side actually binds, never on
// both merely being "some loopback address".
func exposureListenOverlap(a, b exposureListenAddr) bool {
	aSet := exposureListenExpandHost(a.Host)
	bSet := exposureListenExpandHost(b.Host)
	if aSet.Wildcard || bSet.Wildcard {
		return true
	}
	if aSet.Hostname != "" || bSet.Hostname != "" {
		return aSet.Hostname != "" && bSet.Hostname != "" && strings.EqualFold(aSet.Hostname, bSet.Hostname)
	}
	for _, ai := range aSet.IPs {
		for _, bi := range bSet.IPs {
			if ai.Equal(bi) {
				return true
			}
		}
	}
	return false
}

// runExposureListenListenersCollide fails when mcp.listen and admin.listen
// would collide at bind time — a distinct problem from either listener's own
// exposure posture, and one Config.Validate does not catch: it only compares
// the two auth token FILES, never the listen addresses (design §7.3). Only
// the mcp side is gated on transport == "sse": admin.listen is unconditionally
// a TCP address (cmd/serve.go's http.Server{Addr: cfg.Admin.Listen}), but
// mcp.listen is a filesystem socket path under the unix transport, which
// exposureListenParseAddr already reports as unparseable (ok == false) — the
// explicit transport gate just makes that reasoning legible rather than
// relying on SplitHostPort happening to fail on every possible socket path.
func runExposureListenListenersCollide(_ context.Context, in *Input) Result {
	cfg := in.Config
	if cfg.MCP.Transport != "sse" || cfg.MCP.Listen == "" || cfg.Admin.Listen == "" {
		return Pass()
	}
	mcpAddr, ok := exposureListenParseAddr(cfg.MCP.Listen)
	if !ok {
		return Pass()
	}
	adminAddr, ok := exposureListenParseAddr(cfg.Admin.Listen)
	if !ok {
		return Pass()
	}
	if mcpAddr.Port != adminAddr.Port {
		return Pass()
	}
	if !exposureListenOverlap(mcpAddr, adminAddr) {
		return Pass()
	}
	return Found(Finding{
		Severity: SeverityFail,
		Title:    "mcp.listen and admin.listen collide",
		Detail: fmt.Sprintf(
			"mcp.listen %q and admin.listen %q share port %s with overlapping host sets — unequal as strings, "+
				"but one of them will fail to bind (or silently take over the other's port, depending on start order)",
			cfg.MCP.Listen, cfg.Admin.Listen, mcpAddr.Port),
		Remedy: "give mcp.listen and admin.listen distinct ports, or bind them to non-overlapping addresses",
		DocRef: "docs/configuration.qmd#admin",
	})
}
