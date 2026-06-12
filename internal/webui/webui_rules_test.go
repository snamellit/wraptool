package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
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

func TestRuleAddThenDelete(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	// Add a second allow rule so we can delete the first (a tool must keep >=1).
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

func TestRuleDeleteLastAllowRejected(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	before, _ := os.ReadFile(cfgPath)
	rr := postRule(t, srv, "/rules/git/allow/0/delete", url.Values{"expect": {"log"}})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("expected error deleting last allow rule, got %s", loc)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Errorf("config changed despite rejected delete")
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
