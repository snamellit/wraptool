package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/requestlog"
)

func postRule(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Handler().ServeHTTP(rr, r)
	return rr
}

func TestHandleRulesRenders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/rules", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"git", "log", "Allow", "Deny", "/rules/git/allow/add", "/rules/git/allow/0/update"} {
		if !strings.Contains(body, want) {
			t.Errorf("rules page missing %q", want)
		}
	}
}

func TestRuleAdd(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	rr := postRule(t, srv, "/rules/git/allow/add", url.Values{
		"subcommand": {"status"}, "flags": {"--short --branch"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (loc: %s)", rr.Code, rr.Header().Get("Location"))
	}
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Fatalf("add reported error: %s", loc)
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "status") || !strings.Contains(string(got), "--branch") {
		t.Errorf("config not updated:\n%s", got)
	}
}

func TestRuleAddDeny(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	rr := postRule(t, srv, "/rules/git/deny/add", url.Values{"subcommand": {"push --force"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "deny:") || !strings.Contains(string(got), "push") {
		t.Errorf("deny rule not added:\n%s", got)
	}
}

func TestRuleUpdate(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	rr := postRule(t, srv, "/rules/git/allow/0/update", url.Values{
		"expect": {"log"}, "subcommand": {"log"}, "flags": {"--oneline -n"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Fatalf("update reported error: %s", loc)
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "--oneline") {
		t.Errorf("flag not updated:\n%s", got)
	}
}

func TestRuleUpdateDriftRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := postRule(t, srv, "/rules/git/allow/0/update", url.Values{
		"expect": {"commit"}, "subcommand": {"commit"}, "flags": {"-m"},
	})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("expected drift error flash, got %s", loc)
	}
}

func TestRuleUpdateDenyFlags(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	// Set deny_flags on the allow rule, then clear them.
	rr := postRule(t, srv, "/rules/git/allow/0/update", url.Values{
		"expect": {"log"}, "subcommand": {"log"}, "flags": {"--oneline"}, "deny_flags": {"--exec --force"},
	})
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Fatalf("update reported error: %s", loc)
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "deny_flags") || !strings.Contains(string(got), "--exec") {
		t.Errorf("deny_flags not written:\n%s", got)
	}

	// Clearing the field removes the key.
	if rr = postRule(t, srv, "/rules/git/allow/0/update", url.Values{
		"expect": {"log"}, "subcommand": {"log"}, "flags": {"--oneline"}, "deny_flags": {""},
	}); strings.Contains(rr.Header().Get("Location"), "kind=err") {
		t.Fatalf("clear update failed: %s", rr.Header().Get("Location"))
	}
	got, _ = os.ReadFile(cfgPath)
	if strings.Contains(string(got), "deny_flags") {
		t.Errorf("deny_flags not removed when cleared:\n%s", got)
	}
}

func TestRuleAddThenDelete(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	if rr := postRule(t, srv, "/rules/git/allow/add", url.Values{"subcommand": {"status"}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("add: %d", rr.Code)
	}
	// Delete the original [log] rule at index 0.
	rr := postRule(t, srv, "/rules/git/allow/0/delete", url.Values{"expect": {"log"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Fatalf("delete reported error: %s", loc)
	}
	got, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(got), "[log]") {
		t.Errorf("log rule not deleted:\n%s", got)
	}
}

func TestRuleDeleteLastAllowAllowed(t *testing.T) {
	// Emptying a tool of allow rules is permitted — it becomes inert, not open.
	srv, _, cfgPath := newTestServer(t)
	rr := postRule(t, srv, "/rules/git/allow/0/delete", url.Values{"expect": {"log"}})
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Errorf("deleting last allow rule should be allowed, got %s", loc)
	}
	got, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(got), "[log]") {
		t.Errorf("log rule should be deleted:\n%s", got)
	}
}

// addFlagToRule0 gives the base git [log] rule a flag so a constraint can
// target it (SetFlagConstraint validates the flag is allowed on the rule).
func addFlagToRule0(t *testing.T, srv *Server) {
	t.Helper()
	rr := postRule(t, srv, "/rules/git/allow/0/update", url.Values{
		"expect": {"log"}, "subcommand": {"log"}, "flags": {"--oneline"},
	})
	if rr.Code != http.StatusSeeOther || strings.Contains(rr.Header().Get("Location"), "kind=err") {
		t.Fatalf("seeding flag failed: %s", rr.Header().Get("Location"))
	}
}

func TestRuleConstraintSetAndRender(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	addFlagToRule0(t, srv)

	rr := postRule(t, srv, "/rules/git/allow/0/constraint/set", url.Values{
		"expect": {"log"}, "flag": {"--oneline"},
		"type": {"integer"}, "min": {"1"}, "max": {"5"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Fatalf("set constraint reported error: %s", loc)
	}
	got, _ := os.ReadFile(cfgPath)
	for _, want := range []string{"flag_constraints", "--oneline:", "type: integer", "min: 1", "max: 5"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}

	// The rules page renders the editor pre-filled and offers a delete action.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/rules", nil))
	body := rr.Body.String()
	for _, want := range []string{
		"flag constraints",
		"/rules/git/allow/0/constraint/set",
		"/rules/git/allow/0/constraint/delete",
		`value="--oneline"`,
		`value="integer" selected`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rules page missing %q", want)
		}
	}
}

func TestRuleConstraintDelete(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	addFlagToRule0(t, srv)
	if rr := postRule(t, srv, "/rules/git/allow/0/constraint/set", url.Values{
		"expect": {"log"}, "flag": {"--oneline"}, "type": {"string"},
	}); strings.Contains(rr.Header().Get("Location"), "kind=err") {
		t.Fatalf("set failed: %s", rr.Header().Get("Location"))
	}

	rr := postRule(t, srv, "/rules/git/allow/0/constraint/delete", url.Values{
		"expect": {"log"}, "flag": {"--oneline"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Fatalf("delete reported error: %s", loc)
	}
	got, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(got), "flag_constraints") {
		t.Errorf("flag_constraints not removed:\n%s", got)
	}
}

func TestRuleConstraintBadMin(t *testing.T) {
	srv, _, _ := newTestServer(t)
	addFlagToRule0(t, srv)
	rr := postRule(t, srv, "/rules/git/allow/0/constraint/set", url.Values{
		"expect": {"log"}, "flag": {"--oneline"}, "type": {"integer"}, "min": {"abc"},
	})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("expected error for non-integer min, got %s", loc)
	}
}

func TestRuleConstraintEditorAllowOnly(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// Add a deny rule so both an allow and a deny rule are on the page.
	if rr := postRule(t, srv, "/rules/git/deny/add", url.Values{"subcommand": {"push"}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("deny add: %d", rr.Code)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/rules", nil))
	body := rr.Body.String()
	// One allow rule → exactly one constraint editor; the deny rule gets none.
	if got := strings.Count(body, `class="constraints"`); got != 1 {
		t.Errorf("constraint editor count = %d, want 1 (allow rules only)", got)
	}
}

func TestRuleConstraintDeleteEmptyFlagRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := postRule(t, srv, "/rules/git/allow/0/constraint/delete", url.Values{
		"expect": {"log"}, "flag": {""},
	})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("expected error for empty flag on delete, got %s", loc)
	}
}

func TestRuleDeleteAuditsRevocation(t *testing.T) {
	srv, store, _ := newTestServer(t)
	// Add a second allow rule so the delete is permitted, then delete [log].
	if rr := postRule(t, srv, "/rules/git/allow/add", url.Values{"subcommand": {"status"}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("add: %d", rr.Code)
	}
	if rr := postRule(t, srv, "/rules/git/allow/0/delete", url.Values{"expect": {"log"}}); strings.Contains(rr.Header().Get("Location"), "kind=err") {
		t.Fatalf("delete failed: %s", rr.Header().Get("Location"))
	}

	revs, err := store.ListRevocations()
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 1 || revs[0].Tool != "git" || revs[0].Kind != requestlog.RevokeAllowRule {
		t.Fatalf("expected one allow-rule revocation, got %+v", revs)
	}

	// It renders on the unfiltered audit page.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/calls", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "Revocations") || !strings.Contains(body, "revoked allow_rule") {
		t.Errorf("revocation not rendered on /calls:\n%s", body)
	}

	// A call-status filter (allowed/denied) must not show revocations — they are
	// config-change events, not allowed/denied calls.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/calls?status=allowed", nil))
	if strings.Contains(rr.Body.String(), "Revocations") {
		t.Error("revocations should not render under an allowed/denied filter")
	}
}

func TestToolDeleteAuditsRevocation(t *testing.T) {
	srv, store, cfgPath := newTestServer(t)
	// Add a second tool so deleting the first is allowed.
	data, _ := os.ReadFile(cfgPath)
	var binPath string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.Contains(line, "binary:") {
			binPath = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "binary:"))
		}
	}
	if rr := postRule(t, srv, "/tools/add", url.Values{"name": {"kubectl"}, "binary": {binPath}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("tool add: %d", rr.Code)
	}
	if rr := postRule(t, srv, "/tools/git/delete", nil); strings.Contains(rr.Header().Get("Location"), "kind=err") {
		t.Fatalf("tool delete failed: %s", rr.Header().Get("Location"))
	}

	revs, err := store.ListRevocations()
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 1 || revs[0].Kind != requestlog.RevokeTool || revs[0].Tool != "git" {
		t.Fatalf("expected one tool revocation, got %+v", revs)
	}
}

func TestToolAdd(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	// Reuse the existing git binary path so the new tool validates.
	data, _ := os.ReadFile(cfgPath)
	var binPath string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.Contains(line, "binary:") {
			binPath = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "binary:"))
		}
	}
	rr := postRule(t, srv, "/tools/add", url.Values{"name": {"kubectl"}, "binary": {binPath}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Fatalf("add tool reported error: %s", loc)
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "kubectl:") {
		t.Errorf("tool not added:\n%s", got)
	}
}

func TestToolAddMissingBinary(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := postRule(t, srv, "/tools/add", url.Values{"name": {"kubectl"}, "binary": {"/no/such/bin"}})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("expected error for a missing binary, got %s", loc)
	}
}

func TestToolMetadataSetAndRender(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	rr := postRule(t, srv, "/tools/git/metadata", url.Values{
		"working_dir": {"/srv/project"}, "timeout": {"45s"}, "env_files": {"/srv/project/.env"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Fatalf("metadata update reported error: %s", loc)
	}
	got, _ := os.ReadFile(cfgPath)
	for _, want := range []string{"working_dir:", "/srv/project", "timeout: 45s", "env_files:"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	// The page renders the metadata form pre-filled.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/rules", nil))
	body := rr.Body.String()
	for _, want := range []string{`name="working_dir" value="/srv/project"`, `name="timeout" value="45s"`} {
		if !strings.Contains(body, want) {
			t.Errorf("metadata not rendered pre-filled: missing %q", want)
		}
	}
}

func TestToolMetadataBadTimeout(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := postRule(t, srv, "/tools/git/metadata", url.Values{"timeout": {"soon"}})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("expected error for bad timeout, got %s", loc)
	}
}

func TestToolDeleteLastRejected(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	before, _ := os.ReadFile(cfgPath)
	rr := postRule(t, srv, "/tools/git/delete", nil)
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("expected error deleting the last tool, got %s", loc)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Error("config changed despite rejected tool delete")
	}
}

func TestRuleArgsSetAndClear(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	// Set a positional-args limit of 0 (forbids positional args), then clear it.
	rr := postRule(t, srv, "/rules/git/allow/0/args/set", url.Values{
		"expect": {"log"}, "positional_max": {"0"},
	})
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Fatalf("set args reported error: %s", loc)
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "arg_constraints") || !strings.Contains(string(got), "positional_max: 0") {
		t.Errorf("arg_constraints not written:\n%s", got)
	}
	// The page renders the limit pre-filled.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/rules", nil))
	if !strings.Contains(rr.Body.String(), `name="positional_max" value="0"`) {
		t.Errorf("positional_max not rendered pre-filled")
	}

	// Empty field clears the block.
	if rr = postRule(t, srv, "/rules/git/allow/0/args/set", url.Values{
		"expect": {"log"}, "positional_max": {""},
	}); strings.Contains(rr.Header().Get("Location"), "kind=err") {
		t.Fatalf("clear args failed: %s", rr.Header().Get("Location"))
	}
	got, _ = os.ReadFile(cfgPath)
	if strings.Contains(string(got), "arg_constraints") {
		t.Errorf("arg_constraints not removed when cleared:\n%s", got)
	}
}

func TestRuleArgsSetBadValue(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := postRule(t, srv, "/rules/git/allow/0/args/set", url.Values{
		"expect": {"log"}, "positional_max": {"lots"},
	})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("expected error for non-integer positional_max, got %s", loc)
	}
}

func TestRuleAddBadKind(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := postRule(t, srv, "/rules/git/nonsense/add", url.Values{"subcommand": {"x"}})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("expected error for bad kind, got %s", loc)
	}
}

func TestRuleUpdateBadIndex(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := postRule(t, srv, "/rules/git/allow/notanumber/update", url.Values{"subcommand": {"x"}})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "invalid+rule+index") && !strings.Contains(loc, "kind=err") {
		t.Errorf("expected invalid-index error, got %s", loc)
	}
}
