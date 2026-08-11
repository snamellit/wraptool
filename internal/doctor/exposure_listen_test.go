package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// exposureListenInput builds a hand-crafted *Input around cfg. Every check in
// exposure_listen.go reads only in.Config and (exposure.admin-container-reachable)
// in.Env.LookPath — no filesystem fixture is needed, unlike config_load_test.go's
// sanctioned-escape-hatch checks.
func exposureListenInput(cfg *config.Config, env *MapEnv) *Input {
	if env == nil {
		env = NewMapEnv()
	}
	return &Input{
		Env:         env,
		ConfigPath:  "/home/user/.config/wraptool/config.yaml",
		ConfigState: ConfigLoaded,
		Config:      cfg,
	}
}

// ---------------------------------------------------------------- exposure.transport-unsupported

func TestRunExposureListenTransportUnsupported_Supported(t *testing.T) {
	for _, transport := range []string{"sse", "unix"} {
		t.Run(transport, func(t *testing.T) {
			in := exposureListenInput(&config.Config{MCP: config.MCPConfig{Transport: transport}}, nil)
			res := runExposureListenTransportUnsupported(context.Background(), in)
			if res.Outcome() != OutcomePass {
				t.Fatalf("Outcome() = %v, want pass; findings=%+v", res.Outcome(), res.Findings)
			}
		})
	}
}

// TestRunExposureListenTransportUnsupported_NeverNamesTheConfiguredValue
// asserts that Detail/Remedy/Title/Summary/ID never echo back the configured
// transport value, for any unsupported value — which subsumes CLAUDE.md's
// specific rule that wraptool's own test-only transport must never be
// surfaced in user-facing text (docs, examples, help text, scaffolded
// defaults); this finding's text is exactly that surface.
func TestRunExposureListenTransportUnsupported_NeverNamesTheConfiguredValue(t *testing.T) {
	forbidden := []string{"stdio", "STDIO", "grpc", "http2"}
	for _, transport := range forbidden {
		t.Run(transport, func(t *testing.T) {
			in := exposureListenInput(&config.Config{MCP: config.MCPConfig{Transport: transport}}, nil)
			res := runExposureListenTransportUnsupported(context.Background(), in)
			if res.Outcome() != OutcomeFinding {
				t.Fatalf("Outcome() = %v, want finding", res.Outcome())
			}
			f := res.Findings[0]
			for _, text := range []string{f.Title, f.Detail, f.Remedy, string(f.ID)} {
				if strings.Contains(strings.ToLower(text), strings.ToLower(transport)) {
					t.Errorf("finding text %q contains the configured transport value %q — must never echo it back",
						text, transport)
				}
			}
			if strings.Contains(strings.ToLower(exposureListenTransportUnsupportedMeta.Summary), strings.ToLower(transport)) {
				t.Errorf("Meta.Summary %q contains %q", exposureListenTransportUnsupportedMeta.Summary, transport)
			}
			if f.Severity != SeverityWarn {
				t.Errorf("Severity = %q, want warn", f.Severity)
			}
			if f.Remedy == "" {
				t.Error("Remedy is empty for a warn finding")
			}
		})
	}
}

func TestRunExposureListenTransportUnsupported_EmptyTransportWarns(t *testing.T) {
	in := exposureListenInput(&config.Config{}, nil)
	res := runExposureListenTransportUnsupported(context.Background(), in)
	if res.Outcome() != OutcomeFinding {
		t.Fatalf("Outcome() = %v, want finding for an empty transport", res.Outcome())
	}
}

// ---------------------------------------------------------------- exposure.sse-non-loopback

func TestRunExposureListenSSENonLoopback(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.MCPConfig
		want Outcome
	}{
		{
			name: "sse, non-loopback, authenticated -> warn",
			cfg: config.MCPConfig{
				Transport: "sse", Listen: "0.0.0.0:8717", AuthTokenFile: "/token",
			},
			want: OutcomeFinding,
		},
		{
			name: "sse, non-loopback, UNAUTHENTICATED -> pass (Config.Validate's fail owns this)",
			cfg: config.MCPConfig{
				Transport: "sse", Listen: "0.0.0.0:8717",
			},
			want: OutcomePass,
		},
		{
			name: "sse, loopback, authenticated -> pass",
			cfg: config.MCPConfig{
				Transport: "sse", Listen: "127.0.0.1:8717", AuthTokenFile: "/token",
			},
			want: OutcomePass,
		},
		{
			name: "unix transport with a non-loopback-LOOKING listen -> pass (ListenIsLoopback " +
				"would misfire on a socket path if we did not gate on transport)",
			cfg: config.MCPConfig{
				Transport: "unix", Listen: "/run/wraptool/mcp.sock", AuthTokenFile: "/token",
			},
			want: OutcomePass,
		},
		{
			name: "sse, empty listen -> pass",
			cfg: config.MCPConfig{
				Transport: "sse", AuthTokenFile: "/token",
			},
			want: OutcomePass,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := exposureListenInput(&config.Config{MCP: tc.cfg}, nil)
			res := runExposureListenSSENonLoopback(context.Background(), in)
			if res.Outcome() != tc.want {
				t.Fatalf("Outcome() = %v, want %v; findings=%+v", res.Outcome(), tc.want, res.Findings)
			}
			if tc.want == OutcomeFinding {
				f := res.Findings[0]
				if f.Severity != SeverityWarn {
					t.Errorf("Severity = %q, want warn", f.Severity)
				}
				if !strings.Contains(f.Detail, tc.cfg.Listen) {
					t.Errorf("Detail = %q, want it to name the offending listen address", f.Detail)
				}
				if f.Remedy == "" {
					t.Error("Remedy is empty for a warn finding")
				}
			}
		})
	}
}

// ---------------------------------------------------------------- exposure.admin-non-loopback

func TestRunExposureListenAdminNonLoopback(t *testing.T) {
	tests := []struct {
		name   string
		listen string
		want   Outcome
	}{
		{"disabled", "", OutcomePass},
		{"loopback", "127.0.0.1:8718", OutcomePass},
		{"localhost", "localhost:8718", OutcomePass},
		{"wildcard", "0.0.0.0:8718", OutcomeFinding},
		{"specific-remote", "192.168.1.5:8718", OutcomeFinding},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := exposureListenInput(&config.Config{Admin: config.AdminConfig{Listen: tc.listen}}, nil)
			res := runExposureListenAdminNonLoopback(context.Background(), in)
			if res.Outcome() != tc.want {
				t.Fatalf("Outcome() = %v, want %v; findings=%+v", res.Outcome(), tc.want, res.Findings)
			}
			if tc.want == OutcomeFinding {
				f := res.Findings[0]
				if f.Severity != SeverityFail {
					t.Errorf("Severity = %q, want fail (the admin UI is the approval authority)", f.Severity)
				}
				if !strings.Contains(f.Detail, tc.listen) {
					t.Errorf("Detail = %q, want it to name %q", f.Detail, tc.listen)
				}
				if f.Remedy == "" {
					t.Error("Remedy is empty for a fail finding")
				}
			}
		})
	}
}

// ---------------------------------------------------------------- exposure.admin-container-reachable

func TestRunExposureListenAdminContainerReachable(t *testing.T) {
	tests := []struct {
		name    string
		listen  string
		hasGuix bool
		want    Outcome
	}{
		{"admin disabled, guix present -> pass", "", true, OutcomePass},
		{"admin enabled, no guix -> pass", "127.0.0.1:8718", false, OutcomePass},
		{"admin enabled, guix present -> warn", "127.0.0.1:8718", true, OutcomeFinding},
		// The design deliberately does NOT gate this on loopback-ness of
		// admin.listen: a shared netns makes the container count as
		// "localhost" too, so even a loopback bind is reachable from inside.
		{"admin enabled loopback, guix present -> warn regardless", "127.0.0.1:8718", true, OutcomeFinding},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			if tc.hasGuix {
				env.PathBinaries["guix"] = "/usr/bin/guix"
			}
			in := exposureListenInput(&config.Config{Admin: config.AdminConfig{Listen: tc.listen}}, env)
			res := runExposureListenAdminContainerReachable(context.Background(), in)
			if res.Outcome() != tc.want {
				t.Fatalf("Outcome() = %v, want %v; findings=%+v", res.Outcome(), tc.want, res.Findings)
			}
			if tc.want == OutcomeFinding {
				f := res.Findings[0]
				if f.Severity != SeverityWarn {
					t.Errorf("Severity = %q, want warn", f.Severity)
				}
				if !strings.Contains(f.Detail, "guix") {
					t.Errorf("Detail = %q, want it to mention guix", f.Detail)
				}
			}
		})
	}
}

func TestRunExposureListenAdminContainerReachable_NeedsGatesOnWorkspaceNotUsed(t *testing.T) {
	// Design: "Gate on the BACKEND, not on --workspace: the risk is a property
	// of the server config, not of doctor's cwd." Assert the Meta declares no
	// workspace dependency at all.
	if exposureListenAdminContainerReachableMeta.Needs&NeedsWorkspace != 0 {
		t.Errorf("Needs = %v, must not include NeedsWorkspace", exposureListenAdminContainerReachableMeta.Needs)
	}
}

// ---------------------------------------------------------------- exposure.listeners-collide

func TestRunExposureListenListenersCollide(t *testing.T) {
	tests := []struct {
		name        string
		mcpListen   string
		adminListen string
		want        Outcome
	}{
		{
			name:        "wildcard mcp vs loopback admin, same port -> collide",
			mcpListen:   "0.0.0.0:8717",
			adminListen: "127.0.0.1:8717",
			want:        OutcomeFinding,
		},
		{
			name:        "loopback vs loopback, same port -> collide",
			mcpListen:   "127.0.0.1:8717",
			adminListen: "localhost:8717",
			want:        OutcomeFinding,
		},
		{
			name:        "identical specific host, same port -> collide",
			mcpListen:   "192.168.1.5:8717",
			adminListen: "192.168.1.5:8717",
			want:        OutcomeFinding,
		},
		{
			name:        "different ports -> no collision",
			mcpListen:   "127.0.0.1:8717",
			adminListen: "127.0.0.1:8718",
			want:        OutcomePass,
		},
		{
			name:        "different specific hosts, same port -> no collision",
			mcpListen:   "192.168.1.5:8717",
			adminListen: "192.168.1.6:8717",
			want:        OutcomePass,
		},
		{
			name:        "admin disabled -> no collision",
			mcpListen:   "0.0.0.0:8717",
			adminListen: "",
			want:        OutcomePass,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				MCP:   config.MCPConfig{Transport: "sse", Listen: tc.mcpListen},
				Admin: config.AdminConfig{Listen: tc.adminListen},
			}
			in := exposureListenInput(cfg, nil)
			res := runExposureListenListenersCollide(context.Background(), in)
			if res.Outcome() != tc.want {
				t.Fatalf("Outcome() = %v, want %v; findings=%+v", res.Outcome(), tc.want, res.Findings)
			}
			if tc.want == OutcomeFinding {
				f := res.Findings[0]
				if f.Severity != SeverityFail {
					t.Errorf("Severity = %q, want fail", f.Severity)
				}
				if !strings.Contains(f.Detail, tc.mcpListen) || !strings.Contains(f.Detail, tc.adminListen) {
					t.Errorf("Detail = %q, want it to name both listen addresses", f.Detail)
				}
				if f.Remedy == "" {
					t.Error("Remedy is empty for a fail finding")
				}
			}
		})
	}
}

func TestRunExposureListenListenersCollide_UnixTransportNeverCollides(t *testing.T) {
	// mcp.listen is a filesystem socket path under the unix transport, never a
	// TCP address — must not be compared as one even if the path happens to
	// contain a colon.
	cfg := &config.Config{
		MCP:   config.MCPConfig{Transport: "unix", Listen: "/run/user/1000/wraptool:8717"},
		Admin: config.AdminConfig{Listen: "0.0.0.0:8717"},
	}
	in := exposureListenInput(cfg, nil)
	res := runExposureListenListenersCollide(context.Background(), in)
	if res.Outcome() != OutcomePass {
		t.Fatalf("Outcome() = %v, want pass; findings=%+v", res.Outcome(), res.Findings)
	}
}

// ---------------------------------------------------------------- Meta pinning

// TestExposureListenCheckMeta pins every check's ID and Needs against the SAME
// Meta literal init() registers, never through Registered() (plan §1 rule 8;
// mirrors pool_test.go's TestPoolCheckMeta).
func TestExposureListenCheckMeta(t *testing.T) {
	cases := []struct {
		meta      Meta
		wantID    CheckID
		wantNeeds Requirement
	}{
		{exposureListenTransportUnsupportedMeta, exposureListenTransportUnsupportedID, NeedsConfig},
		{exposureListenSSENonLoopbackMeta, exposureListenSSENonLoopbackID, NeedsConfig},
		{exposureListenAdminNonLoopbackMeta, exposureListenAdminNonLoopbackID, NeedsConfig},
		{exposureListenAdminContainerReachableMeta, exposureListenAdminContainerReachableID, NeedsConfig},
		{exposureListenListenersCollideMeta, exposureListenListenersCollideID, NeedsConfig},
	}
	for _, tc := range cases {
		t.Run(string(tc.wantID), func(t *testing.T) {
			if tc.meta.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", tc.meta.ID, tc.wantID)
			}
			if tc.meta.Needs != tc.wantNeeds {
				t.Errorf("Needs = %v, want %v", tc.meta.Needs, tc.wantNeeds)
			}
		})
	}
}
