package doctor

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// locateDoc is a realistic slice of a config document: enough of tools.*,
// mcp.*, and mcp_servers.* to exercise every ConfigPath step kind a group
// file actually builds (see the grep of At/Key/Elem/Item across the package).
// The `flags`/`deny_flags` shape on the "pull" rule mirrors design §6.1's
// worked example exactly, so TestLocateResolvesByValueNotIndex below proves
// the by-value contract against the same scenario the design doc uses to
// justify it.
const locateDoc = `
mcp:
  allowed_cwd_roots:
    - /home/user/project
    - /home/user/other
tools:
  git:
    binary: /usr/bin/git
    working_dir: /home/user/project
    allow:
      - subcommand: [pull]
        flags: ["-c", "--depth"]
        deny_flags: ["-c"]
      - subcommand: [push]
        flags: ["--force"]
  gcloud:
    binary: /usr/bin/gcloud
    allow:
      - subcommand: [compute, instances, list]
        flags: ["--zone"]
mcp_servers:
  acme:
    command: /usr/bin/acme-mcp
    allow:
      - tool: read_thing
      - tool: delete_thing
`

func locateTestLocator() Locator {
	return NewLocator("/home/user/.config/wraptool/config.yaml", []byte(locateDoc))
}

// ---------------------------------------------------------------- basic Key resolution

func TestLocateKeyPathScalarValue(t *testing.T) {
	loc := locateTestLocator()
	site, ok := loc.Locate(At("tools", "git", "binary"))
	if !ok {
		t.Fatal("expected a resolved Site")
	}
	if got := site.String(); got != "/home/user/.config/wraptool/config.yaml:8" {
		t.Errorf("Location = %q, want line 8 (the binary: line)", got)
	}
	if site.KeyNode == nil || site.KeyNode.Value != "binary" {
		t.Errorf("KeyNode = %+v, want the binary key scalar", site.KeyNode)
	}
	if site.ValueNode == nil || site.ValueNode.Value != "/usr/bin/git" {
		t.Errorf("ValueNode = %+v, want the binary scalar value", site.ValueNode)
	}
}

func TestLocateKeyPathNestedSequence(t *testing.T) {
	loc := locateTestLocator()
	site, ok := loc.Locate(At("mcp", "allowed_cwd_roots"))
	if !ok {
		t.Fatal("expected a resolved Site")
	}
	// allowed_cwd_roots: is on line 3; its own key line, not the first list
	// item's line, is what a Key-ending path should report.
	if site.Line != 3 {
		t.Errorf("Line = %d, want 3", site.Line)
	}
}

// ---------------------------------------------------------------- Elem (scalar sequence)

func TestLocateElemScalarSequence(t *testing.T) {
	loc := locateTestLocator()
	site, ok := loc.Locate(At("tools", "git", "allow").Item("subcommand", "pull").Key("flags").Elem("--depth"))
	if !ok {
		t.Fatal("expected a resolved Site")
	}
	if site.KeyNode != nil {
		t.Errorf("KeyNode = %+v, want nil for an Elem-ending path", site.KeyNode)
	}
	if site.ValueNode == nil || site.ValueNode.Value != "--depth" {
		t.Errorf("ValueNode = %+v, want the --depth scalar", site.ValueNode)
	}
}

func TestLocateElemMissingValueUnresolved(t *testing.T) {
	loc := locateTestLocator()
	// "-h"/"--help" are SeedHelpFlags-appended, never present in the file.
	_, ok := loc.Locate(At("tools", "git", "allow").Item("subcommand", "pull").Key("flags").Elem("-h"))
	if ok {
		t.Error("expected an appended, file-absent flag to stay unresolved")
	}
}

// TestLocateResolvesByValueNotIndex is design §6.1's own worked example:
// flags: ["-c", "--depth"] with deny_flags: ["-c"] loads as
// ["--depth", "-h", "--help"] (SeedHelpFlags appends help flags and drops
// deny_flags entries) — so the LOADED struct's index 0 is "--depth" while the
// FILE's index 0 is "-c". A correct locator must find "--depth" at the file's
// real index-1 position by matching its value, never by re-using the loaded
// struct's shifted index.
func TestLocateResolvesByValueNotIndex(t *testing.T) {
	loc := locateTestLocator()
	site, ok := loc.Locate(At("tools", "git", "allow").Item("subcommand", "pull").Key("flags").Elem("--depth"))
	if !ok {
		t.Fatal("expected a resolved Site for the surviving flag")
	}
	if site.Line != 12 {
		t.Errorf("Line = %d, want 12 (the flags: line, where --depth lives)", site.Line)
	}
}

// ---------------------------------------------------------------- Item (mapping sequence)

func TestLocateItemByScalarField(t *testing.T) {
	loc := locateTestLocator()
	site, ok := loc.Locate(At("mcp_servers", "acme", "allow").Item("tool", "delete_thing"))
	if !ok {
		t.Fatal("expected a resolved Site")
	}
	if site.KeyNode != nil {
		t.Errorf("KeyNode = %+v, want nil for an Item-ending path", site.KeyNode)
	}
	if site.ValueNode == nil || site.ValueNode.Kind != yaml.MappingNode {
		t.Errorf("ValueNode = %+v, want the matched allow-rule mapping", site.ValueNode)
	}
}

// TestLocateItemBySequenceFieldJoinsWithSpace is RULING R6: a multi-segment
// subcommand value is always spelled strings.Join(rule.Subcommand, " "), and
// the resolver must join the file's own sequence node the same way.
func TestLocateItemBySequenceFieldJoinsWithSpace(t *testing.T) {
	loc := locateTestLocator()
	site, ok := loc.Locate(At("tools", "gcloud", "allow").Item("subcommand", "compute instances list"))
	if !ok {
		t.Fatal("expected the joined subcommand value to resolve")
	}
	if !strings.HasSuffix(site.File, "config.yaml") {
		t.Errorf("File = %q", site.File)
	}
}

func TestLocateItemWrongJoinSpellingUnresolved(t *testing.T) {
	loc := locateTestLocator()
	// A caller that forgot to join (or joined with the wrong separator) must
	// resolve to nothing, never to the wrong rule — R6's own warning.
	_, ok := loc.Locate(At("tools", "gcloud", "allow").Item("subcommand", "compute,instances,list"))
	if ok {
		t.Error("expected a mis-joined subcommand value to stay unresolved")
	}
}

// ---------------------------------------------------------------- failure modes

func TestLocateUnknownKeyUnresolved(t *testing.T) {
	loc := locateTestLocator()
	_, ok := loc.Locate(At("tools", "git", "does_not_exist"))
	if ok {
		t.Error("expected an absent key to stay unresolved")
	}
}

func TestLocateUnknownToolUnresolved(t *testing.T) {
	loc := locateTestLocator()
	_, ok := loc.Locate(At("tools", "kubectl", "binary"))
	if ok {
		t.Error("expected an absent tool section to stay unresolved")
	}
}

func TestLocateEmptyPathUnresolved(t *testing.T) {
	loc := locateTestLocator()
	_, ok := loc.Locate(ConfigPath{})
	if ok {
		t.Error("expected an empty ConfigPath to stay unresolved")
	}
}

func TestLocateKeyIntoScalarUnresolved(t *testing.T) {
	loc := locateTestLocator()
	// tools.git.binary is a scalar; descending further into it is nonsense.
	_, ok := loc.Locate(At("tools", "git", "binary", "nested"))
	if ok {
		t.Error("expected descending into a scalar to stay unresolved")
	}
}

// ---------------------------------------------------------------- NewLocator edge inputs

func TestNewLocatorEmptyRawNeverResolves(t *testing.T) {
	loc := NewLocator("/x/config.yaml", nil)
	if _, ok := loc.Locate(At("tools", "git", "binary")); ok {
		t.Error("expected nil raw to yield a Locator that resolves nothing")
	}
}

func TestNewLocatorMalformedYAMLNeverResolves(t *testing.T) {
	loc := NewLocator("/x/config.yaml", []byte("tools: [this is not\n  a valid: document"))
	if _, ok := loc.Locate(At("tools")); ok {
		t.Error("expected malformed YAML to yield a Locator that resolves nothing")
	}
}

func TestNewLocatorNonMappingRootNeverResolves(t *testing.T) {
	for _, raw := range []string{"", "null\n", "just a string\n", "- one\n- two\n"} {
		loc := NewLocator("/x/config.yaml", []byte(raw))
		if _, ok := loc.Locate(At("tools", "git", "binary")); ok {
			t.Errorf("raw %q: expected a non-mapping root to yield a Locator that resolves nothing", raw)
		}
	}
}

// TestLocateSatisfiesLocatorInterface pins NewLocator's return type against
// the Locator interface finding.go declares, so a signature drift here is a
// compile error at this test rather than a runtime surprise in doctor.go.
func TestLocateSatisfiesLocatorInterface(t *testing.T) {
	_ = Locator(locateTestLocator())
}

// ---------------------------------------------------------------- duplicate values

func TestLocateElemFirstDuplicateWins(t *testing.T) {
	loc := NewLocator("/x/config.yaml", []byte("tools:\n  git:\n    allow:\n      - subcommand: [pull]\n        flags: [\"-c\", \"-c\"]\n"))
	site, ok := loc.Locate(At("tools", "git", "allow").Item("subcommand", "pull").Key("flags").Elem("-c"))
	if !ok {
		t.Fatal("expected a resolved Site")
	}
	if site.Line != 5 {
		t.Errorf("Line = %d, want 5 (the flags: line, first occurrence)", site.Line)
	}
}
