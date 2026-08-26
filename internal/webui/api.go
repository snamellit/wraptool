package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"

	"github.com/pti/wraptool/internal/adminops"
	"github.com/pti/wraptool/internal/buildinfo"
	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/requestlog"
)

// apiVersion is the wire contract version of /api/v1. Field renames or
// semantic changes require a new /api/v2 — additive fields are fine here.
const apiVersion = 1

// registerAPIRoutes adds the JSON /api/v1 surface to mux. It is a thin,
// encode/decode-only veneer: request/call/rule state comes straight from the
// store and config.Load, and approve/deny logic lives in adminops — nothing
// here duplicates it.
func (s *Server) registerAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/info", s.apiInfo)
	mux.HandleFunc("GET /api/v1/requests", s.apiListRequests)
	mux.HandleFunc("GET /api/v1/requests/{id}", s.apiGetRequest)
	mux.HandleFunc("POST /api/v1/requests/{id}/approve", s.apiApprove)
	mux.HandleFunc("POST /api/v1/requests/{id}/deny", s.apiDeny)
	mux.HandleFunc("GET /api/v1/calls", s.apiListCalls)
	mux.HandleFunc("GET /api/v1/rules", s.apiRules)
	// Catch-all: an unknown path or wrong method under /api/ still gets the
	// JSON error shape, not ServeMux's default text/plain 404/405 (more
	// specific patterns above take precedence regardless of registration
	// order).
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusNotFound, fmt.Errorf("no such API endpoint: %s %s", r.Method, r.URL.Path))
	})
}

func (s *Server) apiInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     buildinfo.Version(),
		"api_version": apiVersion,
	})
}

func (s *Server) apiListRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if _, present := q["status"]; !present {
		status = "pending" // default view; explicit empty (?status=) means "all"
	}

	reqs, err := s.store.List(requestlog.Status(status))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, reqs)
}

func (s *Server) apiGetRequest(w http.ResponseWriter, r *http.Request) {
	req, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeAPIError(w, requestErrorStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// approveBody is the POST /api/v1/requests/{id}/approve request payload.
type approveBody struct {
	Apply     bool              `json:"apply"`
	FlagTypes map[string]string `json:"flag_types"`
}

// approveResponse is the POST /api/v1/requests/{id}/approve response payload.
type approveResponse struct {
	Outcome    string   `json:"outcome"`
	AddedFlags []string `json:"added_flags,omitempty"`
	Snippet    string   `json:"snippet,omitempty"`
	ApplyError string   `json:"apply_error,omitempty"`
}

func (s *Server) apiApprove(w http.ResponseWriter, r *http.Request) {
	var body approveBody
	if err := decodeJSONBody(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}

	result, err := adminops.Approve(s.store, s.configPath, r.PathValue("id"), body.Apply, body.FlagTypes)
	if err != nil {
		writeAPIError(w, requestErrorStatus(err), err)
		return
	}

	resp := approveResponse{
		Outcome:    outcomeString(result.Outcome),
		AddedFlags: result.AddedFlags,
		Snippet:    result.Snippet,
	}
	if result.ApplyErr != nil {
		resp.ApplyError = result.ApplyErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// outcomeString renders an adminops.ApproveOutcome for the wire. The switch is
// exhaustive over the enum (see .golangci.yml's exhaustive linter).
func outcomeString(o adminops.ApproveOutcome) string {
	switch o {
	case adminops.OutcomeNewRule:
		return "new_rule"
	case adminops.OutcomeExtended:
		return "extended"
	case adminops.OutcomeNoChange:
		return "no_change"
	case adminops.OutcomeNotApplied:
		return "not_applied"
	}
	return ""
}

// denyBody is the POST /api/v1/requests/{id}/deny request payload.
type denyBody struct {
	Reason string `json:"reason"`
}

func (s *Server) apiDeny(w http.ResponseWriter, r *http.Request) {
	var body denyBody
	if err := decodeJSONBody(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if err := adminops.Deny(s.store, r.PathValue("id"), body.Reason); err != nil {
		writeAPIError(w, requestErrorStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiListCalls(w http.ResponseWriter, r *http.Request) {
	only := r.URL.Query().Get("status") // "allowed" | "denied" | "" (all)

	calls, err := s.store.ListToolCalls()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}

	out := make([]requestlog.ToolCallEvent, 0, len(calls))
	for _, c := range calls {
		if only == "allowed" && !c.Allowed {
			continue
		}
		if only == "denied" && c.Allowed {
			continue
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- rules snapshot ---
//
// config.FlagConstraint/ArgConstraints carry only yaml tags (the config file
// is the primary format), so a JSON encode of them directly would use bare Go
// field names. These small view structs give the wire format proper
// snake_case json tags instead — a DTO layer here, unlike requests/calls
// which are requestlog's own JSON-tagged structs.
//
// Env/EnvFiles are deliberately omitted: they carry credential material
// (inline secrets or paths to them), out of scope for a policy snapshot.

type apiToolRules struct {
	Name       string         `json:"name"`
	Binary     string         `json:"binary"`
	WorkingDir string         `json:"working_dir,omitempty"`
	Timeout    string         `json:"timeout"`
	Allow      []apiAllowRule `json:"allow,omitempty"`
	Deny       []apiDenyRule  `json:"deny,omitempty"`
}

type apiAllowRule struct {
	Subcommand      []string                     `json:"subcommand"`
	Flags           []string                     `json:"flags,omitempty"`
	DenyFlags       []string                     `json:"deny_flags,omitempty"`
	FlagConstraints map[string]apiFlagConstraint `json:"flag_constraints,omitempty"`
	ArgConstraints  *apiArgConstraints           `json:"arg_constraints,omitempty"`
}

type apiDenyRule struct {
	Subcommand []string `json:"subcommand"`
}

type apiFlagConstraint struct {
	Required bool   `json:"required,omitempty"`
	Type     string `json:"type,omitempty"`
	Max      *int   `json:"max,omitempty"`
	Min      *int   `json:"min,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
	Deny     bool   `json:"deny,omitempty"`
}

type apiArgConstraints struct {
	PositionalMax int `json:"positional_max"`
}

func (s *Server) apiRules(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}

	names := make([]string, 0, len(cfg.Tools))
	for name := range cfg.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	tools := make([]apiToolRules, 0, len(names))
	for _, name := range names {
		tc := cfg.Tools[name]
		tv := apiToolRules{
			Name:       name,
			Binary:     tc.Binary,
			WorkingDir: tc.WorkingDir,
			Timeout:    tc.Timeout.String(),
		}
		for _, rule := range tc.Allow {
			tv.Allow = append(tv.Allow, apiAllowRule{
				Subcommand:      rule.Subcommand,
				Flags:           rule.Flags,
				DenyFlags:       rule.DenyFlags,
				FlagConstraints: apiFlagConstraintsView(rule.FlagConstraints),
				ArgConstraints:  apiArgConstraintsView(rule.ArgConstraints),
			})
		}
		for _, rule := range tc.Deny {
			tv.Deny = append(tv.Deny, apiDenyRule{Subcommand: rule.Subcommand})
		}
		tools = append(tools, tv)
	}
	writeJSON(w, http.StatusOK, tools)
}

func apiFlagConstraintsView(fcs map[string]config.FlagConstraint) map[string]apiFlagConstraint {
	if len(fcs) == 0 {
		return nil
	}
	out := make(map[string]apiFlagConstraint, len(fcs))
	for flag, fc := range fcs {
		out[flag] = apiFlagConstraint{
			Required: fc.Required,
			Type:     string(fc.Type),
			Max:      fc.Max,
			Min:      fc.Min,
			Pattern:  fc.Pattern,
			Deny:     fc.Deny,
		}
	}
	return out
}

func apiArgConstraintsView(ac *config.ArgConstraints) *apiArgConstraints {
	if ac == nil {
		return nil
	}
	return &apiArgConstraints{PositionalMax: ac.PositionalMax}
}

// --- shared JSON helpers ---

// writeJSON encodes v as the response body with status and the JSON content
// type. Encode failures can't be un-written once WriteHeader has run, so they
// are only logged.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("webui: encoding API response: %s", err)
	}
}

func writeAPIError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// decodeJSONBody decodes r's JSON body into v. A missing/empty body is not an
// error — v is left at its zero value (every request body in this API is
// optional: an approve with no body means apply=false, deny with no body
// means an empty reason).
func decodeJSONBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// requestErrorStatus maps a store lookup/update error to an HTTP status:
// requestlog.ErrNotFound (an unknown ID) is a 404; anything else (e.g. a log
// I/O failure) is a 500.
func requestErrorStatus(err error) int {
	if errors.Is(err, requestlog.ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}
