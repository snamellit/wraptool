package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/pti/wraptool/internal/requestlog"
)

func TestAPIListRequestsDefaultsToPending(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pending := appendReq(t, store, "git", []string{"status"}, nil)
	approved := appendReq(t, store, "git", []string{"log"}, nil)
	if err := store.UpdateStatus(approved, requestlog.StatusApproved, ""); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/requests", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got []requestlog.CapabilityRequest
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, rr.Body.String())
	}
	if len(got) != 1 || got[0].ID != pending {
		t.Errorf("default list = %v, want only the pending request %s", got, pending)
	}
}

func TestAPIListRequestsExplicitEmptyMeansAll(t *testing.T) {
	srv, store, _ := newTestServer(t)
	appendReq(t, store, "git", []string{"status"}, nil)
	denied := appendReq(t, store, "git", []string{"log"}, nil)
	if err := store.UpdateStatus(denied, requestlog.StatusDenied, ""); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/requests?status=", nil))
	var got []requestlog.CapabilityRequest
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("?status= list = %d requests, want 2 (all)", len(got))
	}
}

func TestAPIGetRequestNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/requests/req_missing", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["error"] == "" {
		t.Error("expected a non-empty error message")
	}
}

func TestAPIGetRequestFound(t *testing.T) {
	srv, store, _ := newTestServer(t)
	id := appendReq(t, store, "git", []string{"status"}, []string{"--short"})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/requests/"+id, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got requestlog.CapabilityRequest
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != id || got.Tool != "git" {
		t.Errorf("got %+v, want ID %s tool git", got, id)
	}
}

func TestAPIApproveAppliesConfig(t *testing.T) {
	srv, store, cfgPath := newTestServer(t)
	id := appendReq(t, store, "git", []string{"status"}, []string{"--short"})

	body := bytes.NewBufferString(`{"apply": true}`)
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/requests/"+id+"/approve", body)
	r.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp approveResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Outcome != "new_rule" {
		t.Errorf("outcome = %q, want new_rule", resp.Outcome)
	}
	if resp.ApplyError != "" {
		t.Errorf("apply_error = %q, want empty", resp.ApplyError)
	}
	if len(resp.AddedFlags) != 1 || resp.AddedFlags[0] != "--short" {
		t.Errorf("added_flags = %v, want [--short]", resp.AddedFlags)
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != requestlog.StatusApproved {
		t.Errorf("status = %q, want approved", got.Status)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("config file is empty")
	}
}

func TestAPIApproveNotApplied(t *testing.T) {
	srv, store, cfgPath := newTestServer(t)
	id := appendReq(t, store, "git", []string{"status"}, nil)
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/requests/"+id+"/approve", bytes.NewBufferString(`{}`))
	r.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp approveResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Outcome != "not_applied" {
		t.Errorf("outcome = %q, want not_applied", resp.Outcome)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("apply=false must not touch the config file")
	}
}

func TestAPIApproveApplyFailureSurfacesApplyError(t *testing.T) {
	srv, store, _ := newTestServer(t)
	id := appendReq(t, store, "kubectl", []string{"get", "pods"}, nil)

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/requests/"+id+"/approve", bytes.NewBufferString(`{"apply": true}`))
	r.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an apply failure is not itself an HTTP error; body: %s)", rr.Code, rr.Body.String())
	}
	var resp approveResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ApplyError == "" {
		t.Error("apply_error is empty, want a config-apply error for an unconfigured tool")
	}
	if resp.Outcome != "not_applied" {
		t.Errorf("outcome = %q, want not_applied (nothing was actually applied)", resp.Outcome)
	}

	// Status still flips to approved even though the apply failed.
	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != requestlog.StatusApproved {
		t.Errorf("status = %q, want approved", got.Status)
	}
}

func TestAPIApproveUnknownIDIs404(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/requests/req_missing/approve", bytes.NewBufferString(`{}`))
	srv.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestAPIApproveBadBodyIs400(t *testing.T) {
	srv, store, _ := newTestServer(t)
	id := appendReq(t, store, "git", []string{"status"}, nil)

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/requests/"+id+"/approve", bytes.NewBufferString(`{not json`))
	srv.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}

	// The request must be untouched by a rejected body.
	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != requestlog.StatusPending {
		t.Errorf("status = %q, want pending (bad body must not approve)", got.Status)
	}
}

func TestAPIDeny(t *testing.T) {
	srv, store, _ := newTestServer(t)
	id := appendReq(t, store, "git", []string{"push"}, []string{"--force"})

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/requests/"+id+"/deny", bytes.NewBufferString(`{"reason": "too risky"}`))
	r.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body map[string]bool
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body["ok"] {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != requestlog.StatusDenied || got.DenyReason != "too risky" {
		t.Errorf("got status=%q reason=%q, want denied/too risky", got.Status, got.DenyReason)
	}
}

func TestAPIListCallsFilter(t *testing.T) {
	srv, store, _ := newTestServer(t)
	if err := store.AppendToolCall(&requestlog.ToolCallEvent{Tool: "git", Subcommand: []string{"push"}, Allowed: false, DenyReason: "not whitelisted"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendToolCall(&requestlog.ToolCallEvent{Tool: "kubectl", Subcommand: []string{"get", "pods"}, Allowed: true}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/calls?status=denied", nil))
	var calls []requestlog.ToolCallEvent
	if err := json.Unmarshal(rr.Body.Bytes(), &calls); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(calls) != 1 || calls[0].Tool != "git" {
		t.Errorf("denied filter = %v, want just the git call", calls)
	}

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/calls", nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &calls); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(calls) != 2 {
		t.Errorf("unfiltered = %d calls, want 2", len(calls))
	}
}

func TestAPIRulesSnapshot(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/rules", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var tools []apiToolRules
	if err := json.Unmarshal(rr.Body.Bytes(), &tools); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, rr.Body.String())
	}
	if len(tools) != 1 || tools[0].Name != "git" {
		t.Fatalf("tools = %+v, want just git", tools)
	}
	if len(tools[0].Allow) != 1 || tools[0].Allow[0].Subcommand[0] != "log" {
		t.Errorf("allow rules = %+v, want subcommand [log]", tools[0].Allow)
	}
}

func TestAPIInfo(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/info", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["api_version"] != float64(1) {
		t.Errorf("api_version = %v, want 1", body["api_version"])
	}
	if _, ok := body["version"]; !ok {
		t.Error("response missing version")
	}
}

func TestAPIAuthNoTokenIs401JSON(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.AuthMiddleware("s3cret", srv.Handler())

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/requests", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (not the HTML login form)", ct)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", got)
	}
	if sc := rr.Header().Get("Set-Cookie"); sc != "" {
		t.Errorf("Set-Cookie = %q, want none for an API request", sc)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rr.Body.String())
	}
	if body["error"] != "unauthorized" {
		t.Errorf(`error = %q, want "unauthorized"`, body["error"])
	}
}

func TestAPIAuthBearerOK(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.AuthMiddleware("s3cret", srv.Handler())

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/requests", nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestAPIAuthSessionCookieOK(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.AuthMiddleware("s3cret", srv.Handler())

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/requests", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "s3cret"})
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestAPIAuthWrongBearerIsJSON401(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.AuthMiddleware("s3cret", srv.Handler())

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/requests", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (not the HTML login form)", ct)
	}
}

func TestAPIUnknownPathIsJSON404(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/nope", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (not ServeMux's default text/plain)", ct)
	}
}

func TestAPIAuthQueryTokenNotAcceptedForAPI(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.AuthMiddleware("s3cret", srv.Handler())

	// The HTML login flow's ?token= mechanism must not authenticate an /api/
	// request (and must not set a cookie) — only Bearer/cookie do.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/requests?token=s3cret", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (query token must not authenticate the API)", rr.Code)
	}
	if sc := rr.Header().Get("Set-Cookie"); sc != "" {
		t.Errorf("Set-Cookie = %q, want none", sc)
	}
}
