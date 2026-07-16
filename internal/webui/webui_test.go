package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/requestlog"
)

// newTestServer builds a Server backed by a temp request log and config.
func newTestServer(t *testing.T) (*Server, *requestlog.Store, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests.jsonl")
	cfgPath := filepath.Join(dir, "config.yaml")

	// A real executable so config validation (rule edits / ApplyRequest re-run
	// Validate) passes on any host — a hardcoded /usr/bin/git is absent on Guix.
	binPath := filepath.Join(dir, "git")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := "" +
		"server:\n" +
		"  transport: stdio\n" +
		"tools:\n" +
		"  git:\n" +
		"    binary: " + binPath + "\n" +
		"    allow:\n" +
		"      - subcommand: [log]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	store := requestlog.NewStore(logPath)
	srv, err := New(store, cfgPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, store, cfgPath
}

func appendReq(t *testing.T, store *requestlog.Store, tool string, sub, flags []string) string {
	t.Helper()
	req := &requestlog.CapabilityRequest{
		Tool:       tool,
		Subcommand: sub,
		Flags:      flags,
		Rationale:  "need it for CI",
		Enables:    "automated release",
	}
	if err := store.Append(req); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return req.ID
}

func TestHandleListRendersPending(t *testing.T) {
	srv, store, _ := newTestServer(t)
	id := appendReq(t, store, "git", []string{"status"}, nil)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/requests", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{id, "git", "status", "need it for CI", "/requests/" + id + "/approve", "Deny"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleListRendersFlagTypes(t *testing.T) {
	srv, store, _ := newTestServer(t)

	// A request carrying proposed flag types renders them read-only.
	typed := &requestlog.CapabilityRequest{
		Tool:       "glab",
		Subcommand: []string{"mr", "update"},
		Flags:      []string{"--reviewer", "--draft"},
		FlagTypes:  map[string]string{"--reviewer": "string", "--draft": "boolean"},
	}
	if err := store.Append(typed); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// A request without flag types must not render the section.
	plain := appendReq(t, store, "git", []string{"status"}, nil)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/requests", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// A pending typed request renders editable per-flag selects, pre-selected to
	// the proposed type (read-only display is reserved for non-pending cards).
	for _, want := range []string{
		`name="ftype:--reviewer"`, `name="ftype:--draft"`,
		`value="string" selected`, `value="boolean" selected`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// The plain request's card carries no flag-type editor; only the typed
	// request contributes one.
	if got := strings.Count(body, `class="ftedit"`); got != 1 {
		t.Errorf("flag-type editor count = %d, want 1 (plain request %s should have none)", got, plain)
	}
}

func TestHandleListRendersFlagTypesReadOnlyWhenDecided(t *testing.T) {
	srv, store, _ := newTestServer(t)
	req := &requestlog.CapabilityRequest{
		Tool: "glab", Subcommand: []string{"mr", "update"},
		Flags: []string{"--reviewer"}, FlagTypes: map[string]string{"--reviewer": "string"},
	}
	if err := store.Append(req); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.UpdateStatus(req.ID, requestlog.StatusApproved, ""); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/requests?status=approved", nil))
	body := rr.Body.String()
	// Decided requests show the types read-only, with no editable select.
	if !strings.Contains(body, "--reviewer: string") {
		t.Errorf("approved card missing read-only flag types:\n%s", body)
	}
	if strings.Contains(body, `name="ftype:--reviewer"`) {
		t.Error("decided request should not render an editable flag-type select")
	}
}

func TestHandleCallsRenders(t *testing.T) {
	srv, store, _ := newTestServer(t)
	if err := store.AppendToolCall(&requestlog.ToolCallEvent{
		Tool: "git", Subcommand: []string{"push"}, Flags: map[string]string{"--force": ""},
		Allowed: false, DenyReason: "not whitelisted", ClientName: "claude",
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	if err := store.AppendToolCall(&requestlog.ToolCallEvent{
		Tool: "kubectl", Subcommand: []string{"get", "pods"}, Allowed: true, ExitCode: 0,
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/calls", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"git push", "kubectl get pods", "not whitelisted", "claude", "allowed", "denied"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// The denied filter shows only the denied call.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/calls?status=denied", nil))
	body = rr.Body.String()
	if !strings.Contains(body, "git push") || strings.Contains(body, "kubectl get pods") {
		t.Errorf("denied filter wrong; got: %s", body)
	}
}

func TestRootRedirects(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/requests" {
		t.Errorf("Location = %q, want /requests", loc)
	}
}

func TestApproveWithApplyEditsConfig(t *testing.T) {
	srv, store, cfgPath := newTestServer(t)
	id := appendReq(t, store, "git", []string{"status"}, []string{"--short"})

	form := url.Values{"apply": {"1"}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/requests/"+id+"/approve", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Handler().ServeHTTP(rr, r)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != requestlog.StatusApproved {
		t.Errorf("status = %q, want approved", got.Status)
	}

	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "status") {
		t.Errorf("config not updated with new subcommand:\n%s", data)
	}

	// Flash should report success, not error.
	loc := rr.Header().Get("Location")
	if strings.Contains(loc, "kind=err") {
		t.Errorf("approve reported error: %s", loc)
	}
}

func TestApproveOverridesFlagType(t *testing.T) {
	srv, store, cfgPath := newTestServer(t)

	// Requester proposed boolean for --reviewer; operator corrects it to string
	// via the approve form's per-flag select.
	req := &requestlog.CapabilityRequest{
		Tool:       "git",
		Subcommand: []string{"status"},
		Flags:      []string{"--reviewer"},
		FlagTypes:  map[string]string{"--reviewer": "boolean"},
	}
	if err := store.Append(req); err != nil {
		t.Fatalf("Append: %v", err)
	}

	form := url.Values{"apply": {"1"}, "ftype:--reviewer": {"string"}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/requests/"+req.ID+"/approve", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Handler().ServeHTTP(rr, r)

	if loc := rr.Header().Get("Location"); strings.Contains(loc, "kind=err") {
		t.Fatalf("approve reported error: %s", loc)
	}
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "type: string") {
		t.Errorf("operator override not written; config:\n%s", data)
	}
	if strings.Contains(string(data), "type: boolean") {
		t.Errorf("proposed boolean written despite override:\n%s", data)
	}
}

func TestApproveFallsBackToProposedFlagType(t *testing.T) {
	srv, store, cfgPath := newTestServer(t)

	// No ftype override submitted → the requester's proposed type is applied.
	req := &requestlog.CapabilityRequest{
		Tool:       "git",
		Subcommand: []string{"status"},
		Flags:      []string{"--reviewer"},
		FlagTypes:  map[string]string{"--reviewer": "string"},
	}
	if err := store.Append(req); err != nil {
		t.Fatalf("Append: %v", err)
	}

	form := url.Values{"apply": {"1"}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/requests/"+req.ID+"/approve", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Handler().ServeHTTP(rr, r)

	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "type: string") {
		t.Errorf("proposed type not applied without override; config:\n%s", data)
	}
}

func TestApproveUnknownToolReportsError(t *testing.T) {
	srv, store, _ := newTestServer(t)
	id := appendReq(t, store, "kubectl", []string{"get"}, nil)

	form := url.Values{"apply": {"1"}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/requests/"+id+"/approve", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Handler().ServeHTTP(rr, r)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	// Status flips to approved even when apply fails (operator can edit by hand).
	got, _ := store.Get(id)
	if got.Status != requestlog.StatusApproved {
		t.Errorf("status = %q, want approved", got.Status)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "kind=err") {
		t.Errorf("expected error flash for unknown tool, got %s", loc)
	}
}

func TestDenyStoresReason(t *testing.T) {
	srv, store, _ := newTestServer(t)
	id := appendReq(t, store, "git", []string{"push"}, []string{"--force"})

	form := url.Values{"reason": {"too risky"}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/requests/"+id+"/deny", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Handler().ServeHTTP(rr, r)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	got, _ := store.Get(id)
	if got.Status != requestlog.StatusDenied {
		t.Errorf("status = %q, want denied", got.Status)
	}
	if got.DenyReason != "too risky" {
		t.Errorf("DenyReason = %q, want %q", got.DenyReason, "too risky")
	}
}

func TestAuthMiddlewareCookieAndBearer(t *testing.T) {
	srv, _, _ := newTestServer(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := srv.AuthMiddleware("s3cret", next)

	cases := []struct {
		name string
		set  func(*http.Request)
		want int
	}{
		{"no creds shows login", func(*http.Request) {}, http.StatusUnauthorized},
		{"cookie ok", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "s3cret"}) }, http.StatusTeapot},
		{"cookie wrong", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "nope"}) }, http.StatusUnauthorized},
		{"bearer ok", func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3cret") }, http.StatusTeapot},
		{"bearer wrong", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/requests", nil)
			c.set(req)
			h.ServeHTTP(rr, req)
			if rr.Code != c.want {
				t.Errorf("status = %d, want %d", rr.Code, c.want)
			}
		})
	}
}

func TestAuthMiddlewareQueryTokenSetsCookie(t *testing.T) {
	srv, _, _ := newTestServer(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := srv.AuthMiddleware("s3cret", next)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/requests?status=approved&token=s3cret", nil))

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	// Cookie set, token stripped from redirect, other params kept.
	if sc := rr.Header().Get("Set-Cookie"); !strings.Contains(sc, sessionCookie+"=s3cret") || !strings.Contains(sc, "HttpOnly") {
		t.Errorf("Set-Cookie = %q, want HttpOnly session cookie", sc)
	}
	loc := rr.Header().Get("Location")
	if strings.Contains(loc, "token=") {
		t.Errorf("redirect still carries token: %s", loc)
	}
	if !strings.Contains(loc, "status=approved") {
		t.Errorf("redirect dropped status param: %s", loc)
	}
}

func TestAuthMiddlewareFormLogin(t *testing.T) {
	srv, _, _ := newTestServer(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := srv.AuthMiddleware("s3cret", next)

	form := url.Values{"token": {"s3cret"}, "next": {"/requests?status=denied"}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/requests?status=denied" {
		t.Errorf("Location = %q, want honored next field", loc)
	}
}

func TestAuthMiddlewareFormLoginRejectsOpenRedirect(t *testing.T) {
	srv, _, _ := newTestServer(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := srv.AuthMiddleware("s3cret", next)

	form := url.Values{"token": {"s3cret"}, "next": {"https://evil.example/"}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, r)

	if loc := rr.Header().Get("Location"); loc != "/requests" {
		t.Errorf("Location = %q, want /requests (open-redirect blocked)", loc)
	}
}

func TestAuthMiddlewareWrongTokenIs401(t *testing.T) {
	srv, _, _ := newTestServer(t)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h := srv.AuthMiddleware("s3cret", next)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/requests?token=wrong", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Invalid token") {
		t.Error("expected login page to show invalid-token error")
	}
}

func TestAuthMiddlewareStaticBypass(t *testing.T) {
	srv, _, _ := newTestServer(t)
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	h := srv.AuthMiddleware("s3cret", next)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/static/style.css", nil))
	if !called {
		t.Error("/static/ should bypass auth so the login page renders styled")
	}
}

func TestAuthMiddlewareEmptyTokenPassthrough(t *testing.T) {
	srv, _, _ := newTestServer(t)
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	h := srv.AuthMiddleware("", next)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Error("empty token should pass through unauthenticated")
	}
}
