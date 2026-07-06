package mcptools_test

import (
	"context"
	"testing"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/mcptools"
	"github.com/pti/wraptool/internal/whitelist"
)

// allConstraintTypes must list every member of the config.ConstraintType enum.
// ConstraintType.Kind is exhaustive-linted, but this slice is manual — a new
// type added without extending it here escapes the seam test below, so keep
// them in lockstep.
var allConstraintTypes = []config.ConstraintType{
	config.ConstraintUntyped,
	config.ConstraintString,
	config.ConstraintInteger,
	config.ConstraintBoolean,
}

// a value that satisfies each type's enforcement, for the whitelist half.
func sampleValue(ct config.ConstraintType) string {
	k, _ := ct.Kind()
	switch {
	case k.Valueless:
		return "" // a bare flag reaches the policy with an empty value
	case k.Numeric:
		return "42"
	default:
		return "anything"
	}
}

// TestConstraintTypeSeam is the contract test no single package owned: for every
// ConstraintType, one config must be BOTH rendered by mcptools (the presentation
// half) AND accepted by whitelist (the enforcement half). If the two ever derive
// the type's meaning differently again — the drift that caused the boolean bug —
// one half fails here.
func TestConstraintTypeSeam(t *testing.T) {
	for _, ct := range allConstraintTypes {
		label := string(ct)
		if label == "" {
			label = "untyped"
		}
		t.Run(label, func(t *testing.T) {
			k, ok := ct.Kind()
			if !ok {
				t.Fatalf("%q has no Kind — extend ConstraintType.Kind", ct)
			}

			cfg := &config.Config{
				Tools: map[string]config.ToolConfig{
					"tool": {
						Binary: "/nonexistent/tool", // introspection fails → config-only
						Allow: []config.AllowRule{{
							Subcommand:      []string{"run"},
							Flags:           []string{"--x"},
							FlagConstraints: map[string]config.FlagConstraint{"--x": {Type: ct}},
						}},
					},
				},
			}
			tc := cfg.Tools["tool"]
			pol := whitelist.NewPolicy(map[string]*config.ToolConfig{"tool": &tc})

			// Presentation half — mcptools renders the flag with the schema type
			// Kind reports.
			src := mcptools.NewCLISource(cfg, pol, nil)
			tools, err := src.Tools(context.Background())
			if err != nil {
				t.Fatalf("mcptools Tools(): %v", err)
			}
			name := mcptools.ToolName("tool", []string{"run"})
			var props map[string]any
			for _, rt := range tools {
				if rt.Name == name {
					props = rt.Tool.InputSchema.Properties
					break
				}
			}
			if props == nil {
				t.Fatalf("mcptools did not register %q", name)
			}
			prop, ok := props["x"].(map[string]any)
			if !ok {
				t.Fatalf("param x missing/misshapen: %v", props["x"])
			}
			// The seam that matters is the valueless axis — the boolean-bug class.
			// mcptools renders a bare boolean param iff Kind says valueless; the
			// exact non-boolean token (mcp-go emits "number" for integers) is an
			// mcptools rendering detail, not part of the contract.
			renderedBool := prop["type"] == "boolean"
			if renderedBool != k.Valueless {
				t.Errorf("presentation drift: mcptools rendered boolean=%v, Kind.Valueless=%v", renderedBool, k.Valueless)
			}

			// Enforcement half — whitelist accepts a valid call for the same flag,
			// and never rejects it as an unknown constraint type.
			d := pol.Evaluate("tool", []string{"run"}, map[string]string{"--x": sampleValue(ct)}, nil)
			if !d.Allowed {
				t.Errorf("enforcement: whitelist denied a valid %q call: %s", ct, d.Reason)
			}
		})
	}
}
