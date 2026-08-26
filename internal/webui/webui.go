// Package webui serves the operator-facing capability-request review UI.
// It runs inside the serve process on its own listener, reads the JSONL
// request log, and applies approvals to the YAML config via
// config.ApplyRequest — the running server then hot-reloads via fsnotify.
package webui

import (
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/pti/wraptool/internal/adminops"
	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/requestlog"
)

//go:embed templates
var assets embed.FS

// tmplFuncs provides a minimal `dict` helper so a partial template can be
// invoked with several named values (Go's html/template passes a single
// pipeline argument to {{template}}).
var tmplFuncs = template.FuncMap{
	// emptyConstraint yields a zero constraintView for the "add constraint" form
	// so it can reuse the same partial as the pre-filled edit forms.
	"emptyConstraint": func() constraintView { return constraintView{} },
	"dict": func(pairs ...any) (map[string]any, error) {
		if len(pairs)%2 != 0 {
			return nil, fmt.Errorf("dict requires an even number of arguments")
		}
		m := make(map[string]any, len(pairs)/2)
		for i := 0; i < len(pairs); i += 2 {
			key, ok := pairs[i].(string)
			if !ok {
				return nil, fmt.Errorf("dict key %d is not a string", i)
			}
			m[key] = pairs[i+1]
		}
		return m, nil
	},
}

// Server holds the dependencies for the Web UI request handlers.
type Server struct {
	store      *requestlog.Store
	configPath string
	tmpl       *template.Template
	static     http.Handler
}

// New builds a Server. configPath is the resolved config file path used for
// applying approved capabilities (must match the path the serve process
// loaded, so file-watch reload picks up the edit).
func New(store *requestlog.Store, configPath string) (*Server, error) {
	tmpl, err := template.New("webui").Funcs(tmplFuncs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing templates: %w", err)
	}
	sub, err := fs.Sub(assets, "templates")
	if err != nil {
		return nil, fmt.Errorf("sub fs: %w", err)
	}
	return &Server{
		store:      store,
		configPath: configPath,
		tmpl:       tmpl,
		static:     http.StripPrefix("/static/", http.FileServerFS(sub)),
	}, nil
}

// Handler returns the unauthenticated route mux. Wrap it with AuthMiddleware
// when a token is configured.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/requests", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /requests", s.handleList)
	mux.HandleFunc("POST /requests/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /requests/{id}/deny", s.handleDeny)
	mux.HandleFunc("GET /calls", s.handleCalls)
	mux.HandleFunc("GET /rules", s.handleRules)
	mux.HandleFunc("POST /tools/add", s.handleToolAdd)
	mux.HandleFunc("POST /tools/{tool}/delete", s.handleToolDelete)
	mux.HandleFunc("POST /tools/{tool}/metadata", s.handleToolMetadata)
	mux.HandleFunc("POST /rules/{tool}/{kind}/add", s.handleRuleAdd)
	mux.HandleFunc("POST /rules/{tool}/{kind}/{index}/update", s.handleRuleUpdate)
	mux.HandleFunc("POST /rules/{tool}/{kind}/{index}/delete", s.handleRuleDelete)
	// flag_constraints are allow-only, so these routes are not parameterised on kind.
	mux.HandleFunc("POST /rules/{tool}/allow/{index}/constraint/set", s.handleRuleConstraintSet)
	mux.HandleFunc("POST /rules/{tool}/allow/{index}/constraint/delete", s.handleRuleConstraintDelete)
	mux.HandleFunc("POST /rules/{tool}/allow/{index}/args/set", s.handleRuleArgsSet)
	mux.Handle("GET /static/", s.static)
	s.registerAPIRoutes(mux)
	return mux
}

// --- view models ---

type filterView struct {
	Label  string
	Value  string
	Active bool
}

type requestView struct {
	requestlog.CapabilityRequest
	When string
}

type flashView struct {
	Kind    string // "ok" | "err"
	Message string
	Detail  string
}

type listView struct {
	Filters     []filterView
	Requests    []requestView
	StatusLabel string
	Flash       *flashView
}

type callView struct {
	requestlog.ToolCallEvent
	When    string
	Command string
}

type callsView struct {
	Filters     []filterView
	Calls       []callView
	Revocations []revocationView
	StatusOnly  string // "allowed" | "denied" | "" (all)
	Flash       *flashView
}

type revocationView struct {
	When   string
	Kind   string // "tool" | "allow_rule" | "deny_rule"
	Target string // e.g. "git status" or "git" for a whole tool
	ID     string
}

var filters = []struct{ Label, Value string }{
	{"Pending", "pending"},
	{"Approved", "approved"},
	{"Denied", "denied"},
	{"All", ""},
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if _, present := q["status"]; !present {
		status = "pending" // default view; explicit empty (?status=) means "all"
	}

	reqs, err := s.store.List(requestlog.Status(status))
	if err != nil {
		http.Error(w, "reading requests: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Newest first.
	views := make([]requestView, len(reqs))
	for i, req := range reqs {
		views[i] = requestView{
			CapabilityRequest: req,
			When:              req.Timestamp.Format("2006-01-02 15:04 MST"),
		}
	}
	slices.Reverse(views)

	statusLabel := "pending"
	fviews := make([]filterView, len(filters))
	for i, f := range filters {
		fviews[i] = filterView{Label: f.Label, Value: f.Value, Active: f.Value == status}
		if f.Value == status {
			statusLabel = strings.ToLower(f.Label)
		}
	}
	if status == "" {
		statusLabel = ""
	}

	data := listView{
		Filters:     fviews,
		Requests:    views,
		StatusLabel: statusLabel,
		Flash:       flashFromQuery(q),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "page", data); err != nil {
		log.Printf("webui: render: %s", err)
	}
}

// callFilters mirror the request filters but partition on the allow decision.
var callFilters = []struct{ Label, Value string }{
	{"All", ""},
	{"Allowed", "allowed"},
	{"Denied", "denied"},
}

func (s *Server) handleCalls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	only := q.Get("status") // "allowed" | "denied" | "" (all)

	calls, err := s.store.ListToolCalls()
	if err != nil {
		http.Error(w, "reading tool calls: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Allowed is the authoritative decision; DenyReason is only its explanation
	// (AppendToolCall guarantees an allowed event carries no reason), so classify
	// purely on Allowed.
	views := make([]callView, 0, len(calls))
	for _, c := range calls {
		if only == "allowed" && !c.Allowed {
			continue
		}
		if only == "denied" && c.Allowed {
			continue
		}
		views = append(views, callView{
			ToolCallEvent: c,
			When:          c.Timestamp.Format("2006-01-02 15:04:05 MST"),
			Command:       formatCommand(c),
		})
	}
	slices.Reverse(views) // newest first

	fviews := make([]filterView, len(callFilters))
	for i, f := range callFilters {
		fviews[i] = filterView{Label: f.Label, Value: f.Value, Active: f.Value == only}
	}

	// Revocations are config-change events, not allowed/denied calls, so they
	// belong only on the unfiltered ("All") view — showing them under an
	// allowed/denied filter would misrepresent them.
	var revocations []revocationView
	if only == "" {
		revocations, err = s.revocationViews()
		if err != nil {
			// The audit trail failing to read is not silently glossed — the
			// operator must know the view is incomplete.
			http.Error(w, "reading revocations: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	data := callsView{
		Filters:     fviews,
		Calls:       views,
		Revocations: revocations,
		StatusOnly:  only,
		Flash:       flashFromQuery(q),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "calls", data); err != nil {
		log.Printf("webui: render calls: %s", err)
	}
}

// revocationViews reads the revocation audit trail, newest first. The read error
// is returned (not swallowed) so the caller can surface an incomplete audit view
// rather than silently showing an empty one.
func (s *Server) revocationViews() ([]revocationView, error) {
	revs, err := s.store.ListRevocations()
	if err != nil {
		return nil, err
	}
	views := make([]revocationView, 0, len(revs))
	for _, rv := range revs {
		target := rv.Tool
		if len(rv.Subcommand) > 0 {
			target += " " + strings.Join(rv.Subcommand, " ")
		}
		views = append(views, revocationView{
			When:   rv.Timestamp.Format("2006-01-02 15:04:05 MST"),
			Kind:   string(rv.Kind),
			Target: target,
			ID:     rv.ID,
		})
	}
	slices.Reverse(views) // newest first
	return views, nil
}

// formatCommand renders a tool call as an approximate command line for display.
func formatCommand(c requestlog.ToolCallEvent) string {
	parts := append([]string{c.Tool}, c.Subcommand...)
	// Flags is a map; sort keys for stable output.
	keys := make([]string, 0, len(c.Flags))
	for k := range c.Flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := c.Flags[k]; v != "" {
			parts = append(parts, k, v)
		} else {
			parts = append(parts, k)
		}
	}
	parts = append(parts, c.PositionalArgs...)
	return strings.Join(parts, " ")
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	apply := r.FormValue("apply") == "1"

	// Fetched here (adminops.Approve fetches it again internally) purely to
	// read FlagTypes for the merge below and Tool/Subcommand for the success
	// message — and, as a side effect, to reproduce the original unprefixed
	// "unknown id" message rather than adminops's single collapsed error.
	req, err := s.store.Get(id)
	if err != nil {
		s.redirect(w, r, "/requests", "err", err.Error(), "")
		return
	}

	result, err := adminops.Approve(s.store, s.configPath, id, apply, flagTypeOverrides(req.FlagTypes, r))
	if err != nil {
		s.redirect(w, r, "/requests", "err", err.Error(), "")
		return
	}

	if result.Outcome == adminops.OutcomeNotApplied {
		s.redirect(w, r, "/requests", "ok", fmt.Sprintf("Approved %s. Add the capability to config manually; a running wraptool hot-reloads on save.", id), "")
		return
	}

	if result.ApplyErr != nil {
		// Status is already approved; report the apply failure so the
		// operator can edit the config by hand.
		s.redirect(w, r, "/requests", "err", fmt.Sprintf("Approved %s but applying to config failed: %s", id, result.ApplyErr), "")
		return
	}

	switch result.Outcome {
	case adminops.OutcomeNewRule:
		s.redirect(w, r, "/requests", "ok", fmt.Sprintf("Approved %s — added allow rule for %s %s. Server will hot-reload.", id, req.Tool, strings.Join(req.Subcommand, " ")), result.Snippet)
	case adminops.OutcomeExtended:
		s.redirect(w, r, "/requests", "ok", fmt.Sprintf("Approved %s — extended rule with flags %s. Server will hot-reload.", id, strings.Join(result.AddedFlags, " ")), result.Snippet)
	case adminops.OutcomeNoChange:
		s.redirect(w, r, "/requests", "ok", fmt.Sprintf("Approved %s — capability was already allowed; no config change.", id), "")
	case adminops.OutcomeNotApplied:
		// unreachable: handled above before the apply-error/outcome branch
	}
}

// flagTypeOverrides merges the operator's per-flag type selections (form
// fields named "ftype:<flag>", submitted by the approve form) over the
// requester's proposed FlagTypes, so the operator's choice at approval time
// wins. A submitted empty value clears the proposal — applyFlagTypes writes no
// constraint for an empty type, leaving that flag unconstrained. Returns nil
// when there is nothing to apply, matching the pre-override behaviour.
//
// Reads r.PostForm (POST body only), not r.Form: an approve is a POST, so a
// stray ?ftype:... query parameter must not influence the applied types.
// ParseForm is called explicitly (idempotent) so PostForm is populated
// regardless of whether the caller already touched the form.
func flagTypeOverrides(proposed map[string]string, r *http.Request) map[string]string {
	if err := r.ParseForm(); err != nil {
		// Non-fatal: proceed with whatever PostForm holds, but leave a trace so
		// a mangled body that drops the operator's overrides is diagnosable.
		log.Printf("webui: parsing approve form: %s", err)
	}
	merged := make(map[string]string, len(proposed))
	maps.Copy(merged, proposed)
	for key, vals := range r.PostForm {
		flag, ok := strings.CutPrefix(key, "ftype:")
		if !ok || flag == "" || len(vals) == 0 {
			continue // skip non-ftype keys and a malformed "ftype:" with no flag
		}
		merged[flag] = vals[0]
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func (s *Server) handleDeny(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reason := strings.TrimSpace(r.FormValue("reason"))
	if err := adminops.Deny(s.store, id, reason); err != nil {
		s.redirect(w, r, "/requests", "err", "denying: "+err.Error(), "")
		return
	}
	s.redirect(w, r, "/requests", "ok", fmt.Sprintf("Denied %s.", id), "")
}

// redirect issues a POST/redirect/GET to dest, carrying a one-shot flash
// message through query params.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, dest, kind, msg, detail string) {
	q := url.Values{}
	q.Set("kind", kind)
	q.Set("msg", msg)
	if detail != "" {
		q.Set("detail", detail)
	}
	http.Redirect(w, r, dest+"?"+q.Encode(), http.StatusSeeOther)
}

func flashFromQuery(q url.Values) *flashView {
	msg := q.Get("msg")
	if msg == "" {
		return nil
	}
	kind := q.Get("kind")
	if kind != "err" {
		kind = "ok"
	}
	return &flashView{Kind: kind, Message: msg, Detail: q.Get("detail")}
}

// --- rule CRUD ---

type ruleView struct {
	Kind        string // "allow" | "deny"
	Index       int
	Subcommand  string // space-joined tokens
	Flags       string // space-joined tokens
	DenyFlags   string // space-joined tokens
	ArgMax      string // arg_constraints.positional_max, "" when unset (unlimited)
	Constraints []constraintView
}

// constraintView is a flag_constraints entry rendered for the rules editor.
// Numeric bounds are strings so an unset bound renders as an empty input.
type constraintView struct {
	Flag     string
	Type     string
	Min      string
	Max      string
	Pattern  string
	Required bool
	Deny     bool
}

type toolRulesView struct {
	Name       string
	Binary     string
	WorkingDir string
	Timeout    string // effective duration, e.g. "30s"
	EnvFiles   string // newline-joined paths (a path may contain spaces)
	Allow      []ruleView
	Deny       []ruleView
}

type rulesView struct {
	Tools []toolRulesView
	Flash *flashView
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		http.Error(w, "loading config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	names := make([]string, 0, len(cfg.Tools))
	for name := range cfg.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	tools := make([]toolRulesView, 0, len(names))
	for _, name := range names {
		tc := cfg.Tools[name]
		tv := toolRulesView{
			Name:       name,
			Binary:     tc.Binary,
			WorkingDir: tc.WorkingDir,
			Timeout:    tc.Timeout.String(),
			EnvFiles:   strings.Join(tc.EnvFiles, "\n"), // one path per line (paths may contain spaces)
		}
		for i, rule := range tc.Allow {
			tv.Allow = append(tv.Allow, ruleView{
				Kind:        config.KindAllow,
				Index:       i,
				Subcommand:  strings.Join(rule.Subcommand, " "),
				Flags:       strings.Join(rule.Flags, " "),
				DenyFlags:   strings.Join(rule.DenyFlags, " "),
				ArgMax:      argMaxString(rule.ArgConstraints),
				Constraints: constraintViews(rule.FlagConstraints),
			})
		}
		for i, rule := range tc.Deny {
			tv.Deny = append(tv.Deny, ruleView{
				Kind:       config.KindDeny,
				Index:      i,
				Subcommand: strings.Join(rule.Subcommand, " "),
			})
		}
		tools = append(tools, tv)
	}

	data := rulesView{Tools: tools, Flash: flashFromQuery(r.URL.Query())}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "rules", data); err != nil {
		log.Printf("webui: render rules: %s", err)
	}
}

func (s *Server) handleToolAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	binary := strings.TrimSpace(r.FormValue("binary"))
	if err := config.AddTool(s.configPath, name, binary); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Add tool failed: %s", err), "")
		return
	}
	s.redirect(w, r, "/rules", "ok", fmt.Sprintf("Added tool %s. Add allow rules to expose it; the server will hot-reload.", name), "")
}

func (s *Server) handleToolMetadata(w http.ResponseWriter, r *http.Request) {
	tool := r.PathValue("tool")
	workingDir := strings.TrimSpace(r.FormValue("working_dir"))
	timeout := strings.TrimSpace(r.FormValue("timeout"))
	// One path per line, not space-separated: an env-file path may contain
	// spaces, so splitting on whitespace would shred it on the round-trip.
	envFiles := splitLines(r.FormValue("env_files"))

	if err := config.SetToolMetadata(s.configPath, tool, workingDir, timeout, envFiles); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Update %s metadata failed: %s", tool, err), "")
		return
	}
	s.redirect(w, r, "/rules", "ok", fmt.Sprintf("Updated metadata for tool %s. Server will hot-reload.", tool), "")
}

// splitLines splits a textarea value into trimmed, non-empty lines. Used for
// list inputs whose items may contain spaces (e.g. env-file paths), where a
// whitespace split would break an item.
func splitLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (s *Server) handleToolDelete(w http.ResponseWriter, r *http.Request) {
	tool := r.PathValue("tool")
	if err := config.DeleteTool(s.configPath, tool); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Delete tool %s failed: %s", tool, err), "")
		return
	}
	if err := s.auditRevocation(&requestlog.RevocationEvent{Tool: tool, Kind: requestlog.RevokeTool}); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Deleted tool %s, but recording it to the audit log FAILED — note this manually: %s", tool, err), "")
		return
	}
	s.redirect(w, r, "/rules", "ok", fmt.Sprintf("Deleted tool %s. Server will hot-reload.", tool), "")
}

func (s *Server) handleRuleAdd(w http.ResponseWriter, r *http.Request) {
	tool := r.PathValue("tool")
	kind := r.PathValue("kind")
	subcommand := strings.Fields(r.FormValue("subcommand"))
	flags := strings.Fields(r.FormValue("flags"))

	if err := config.AddRule(s.configPath, tool, kind, subcommand, flags); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Add %s rule failed: %s", kind, err), "")
		return
	}
	s.redirect(w, r, "/rules", "ok", fmt.Sprintf("Added %s rule %s %s. Server will hot-reload.", kind, tool, strings.Join(subcommand, " ")), "")
}

func (s *Server) handleRuleUpdate(w http.ResponseWriter, r *http.Request) {
	tool := r.PathValue("tool")
	kind := r.PathValue("kind")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		s.redirect(w, r, "/rules", "err", "invalid rule index", "")
		return
	}
	expect := strings.Fields(r.FormValue("expect"))
	subcommand := strings.Fields(r.FormValue("subcommand"))
	flags := strings.Fields(r.FormValue("flags"))
	denyFlags := strings.Fields(r.FormValue("deny_flags"))

	if err := config.UpdateRule(s.configPath, tool, kind, index, expect, subcommand, flags, denyFlags); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Update %s rule failed: %s", kind, err), "")
		return
	}
	s.redirect(w, r, "/rules", "ok", fmt.Sprintf("Updated %s rule %s %s. Server will hot-reload.", kind, tool, strings.Join(subcommand, " ")), "")
}

func (s *Server) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	tool := r.PathValue("tool")
	kind := r.PathValue("kind")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		s.redirect(w, r, "/rules", "err", "invalid rule index", "")
		return
	}
	expect := strings.Fields(r.FormValue("expect"))

	if err := config.DeleteRule(s.configPath, tool, kind, index, expect); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Delete %s rule failed: %s", kind, err), "")
		return
	}
	revKind := requestlog.RevokeAllowRule
	if kind == config.KindDeny {
		revKind = requestlog.RevokeDenyRule
	}
	if err := s.auditRevocation(&requestlog.RevocationEvent{Tool: tool, Kind: revKind, Subcommand: expect}); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Deleted %s rule %s, but recording it to the audit log FAILED — note this manually: %s", kind, tool, err), "")
		return
	}
	s.redirect(w, r, "/rules", "ok", fmt.Sprintf("Deleted %s rule %s. Server will hot-reload.", kind, tool), "")
}

// auditRevocation records a capability removal in the request log. The config
// edit has already succeeded and cannot be rolled back, so a failure here can't
// block it — but an audit trail that loses records silently is worse than none,
// so the error is returned for the caller to surface to the operator (and
// logged) rather than swallowed.
func (s *Server) auditRevocation(evt *requestlog.RevocationEvent) error {
	if err := s.store.AppendRevocation(evt); err != nil {
		log.Printf("webui: recording revocation: %s", err)
		return err
	}
	return nil
}

// argMaxString renders a rule's positional-arg limit for the editor: the number
// when arg_constraints is set (including 0, which forbids positional args), or
// "" when unset (unlimited).
func argMaxString(ac *config.ArgConstraints) string {
	if ac == nil {
		return ""
	}
	return strconv.Itoa(ac.PositionalMax)
}

// constraintViews renders a rule's flag_constraints map into a stable,
// flag-sorted slice for the template. An unset numeric bound becomes an empty
// string so its input renders blank.
func constraintViews(fcs map[string]config.FlagConstraint) []constraintView {
	if len(fcs) == 0 {
		return nil
	}
	flags := make([]string, 0, len(fcs))
	for f := range fcs {
		flags = append(flags, f)
	}
	sort.Strings(flags)
	views := make([]constraintView, 0, len(flags))
	for _, f := range flags {
		fc := fcs[f]
		cv := constraintView{
			Flag:     f,
			Type:     string(fc.Type),
			Pattern:  fc.Pattern,
			Required: fc.Required,
			Deny:     fc.Deny,
		}
		if fc.Min != nil {
			cv.Min = strconv.Itoa(*fc.Min)
		}
		if fc.Max != nil {
			cv.Max = strconv.Itoa(*fc.Max)
		}
		views = append(views, cv)
	}
	return views
}

// parseFlagConstraintForm reads a flag_constraints entry from an approve/rule
// form. An invalid type is not rejected here — SetFlagConstraint's round-trip
// validation catches it against the closed ConstraintType enum — but a
// non-integer min/max is a parse error surfaced to the operator.
func parseFlagConstraintForm(r *http.Request) (config.FlagConstraint, error) {
	fc := config.FlagConstraint{
		Type:     config.ConstraintType(strings.TrimSpace(r.FormValue("type"))),
		Pattern:  strings.TrimSpace(r.FormValue("pattern")),
		Required: r.FormValue("required") == "1",
		Deny:     r.FormValue("deny") == "1",
	}
	parseInt := func(name string) (*int, error) {
		v := strings.TrimSpace(r.FormValue(name))
		if v == "" {
			return nil, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%s must be an integer: %w", name, err)
		}
		return &n, nil
	}
	var err error
	if fc.Min, err = parseInt("min"); err != nil {
		return fc, err
	}
	if fc.Max, err = parseInt("max"); err != nil {
		return fc, err
	}
	return fc, nil
}

func (s *Server) handleRuleConstraintSet(w http.ResponseWriter, r *http.Request) {
	tool := r.PathValue("tool")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		s.redirect(w, r, "/rules", "err", "invalid rule index", "")
		return
	}
	expect := strings.Fields(r.FormValue("expect"))
	flag := strings.TrimSpace(r.FormValue("flag"))
	fc, err := parseFlagConstraintForm(r)
	if err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Set constraint failed: %s", err), "")
		return
	}
	if err := config.SetFlagConstraint(s.configPath, tool, index, expect, flag, fc); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Set constraint failed: %s", err), "")
		return
	}
	s.redirect(w, r, "/rules", "ok", fmt.Sprintf("Set constraint on %s %s. Server will hot-reload.", tool, flag), "")
}

func (s *Server) handleRuleArgsSet(w http.ResponseWriter, r *http.Request) {
	tool := r.PathValue("tool")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		s.redirect(w, r, "/rules", "err", "invalid rule index", "")
		return
	}
	expect := strings.Fields(r.FormValue("expect"))
	// An empty field means "unlimited" — remove the arg_constraints block.
	var positionalMax *int
	if v := strings.TrimSpace(r.FormValue("positional_max")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.redirect(w, r, "/rules", "err", fmt.Sprintf("Set arg limit failed: positional max must be an integer: %s", err), "")
			return
		}
		positionalMax = &n
	}
	if err := config.SetArgConstraints(s.configPath, tool, index, expect, positionalMax); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Set arg limit failed: %s", err), "")
		return
	}
	s.redirect(w, r, "/rules", "ok", fmt.Sprintf("Set positional arg limit on %s. Server will hot-reload.", tool), "")
}

func (s *Server) handleRuleConstraintDelete(w http.ResponseWriter, r *http.Request) {
	tool := r.PathValue("tool")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		s.redirect(w, r, "/rules", "err", "invalid rule index", "")
		return
	}
	expect := strings.Fields(r.FormValue("expect"))
	flag := strings.TrimSpace(r.FormValue("flag"))
	if flag == "" {
		s.redirect(w, r, "/rules", "err", "Delete constraint failed: flag name is required", "")
		return
	}

	if err := config.DeleteFlagConstraint(s.configPath, tool, index, expect, flag); err != nil {
		s.redirect(w, r, "/rules", "err", fmt.Sprintf("Delete constraint failed: %s", err), "")
		return
	}
	s.redirect(w, r, "/rules", "ok", fmt.Sprintf("Deleted constraint on %s %s. Server will hot-reload.", tool, flag), "")
}

const sessionCookie = "wraptool_session"

// AuthMiddleware guards next with a shared token. Authentication is accepted,
// in order, from: a valid session cookie; an "Authorization: Bearer <token>"
// header (for scripts); or a `token` value supplied via query param (GET link)
// or login form (POST). On a valid token the middleware sets an HttpOnly
// session cookie and redirects to a clean URL, so subsequent requests carry
// the cookie. When unauthenticated, browsers get a login form; the /static/
// assets are served without auth so that form renders styled. All token
// comparisons are constant-time. An empty token is a configuration error at
// startup; reject it here as defense in depth rather than trusting loopback.
func (s *Server) AuthMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Web UI authentication is not configured", http.StatusServiceUnavailable)
		})
	}
	want := []byte(token)
	ok := func(got string) bool {
		return subtle.ConstantTimeCompare([]byte(got), want) == 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static assets (CSS) carry no secrets — serve unauthenticated so
		// the login page is styled.
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}

		// 1. Existing session cookie.
		if c, err := r.Cookie(sessionCookie); err == nil && ok(c.Value) {
			next.ServeHTTP(w, r)
			return
		}

		// 2. Bearer header (scripts/curl).
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			if ok(strings.TrimPrefix(auth, "Bearer ")) {
				next.ServeHTTP(w, r)
				return
			}
		}

		// API clients get neither the login form nor a session cookie: an
		// unauthenticated /api/ request fails with a JSON 401 right here,
		// never falling through to the query/form-token flow below (that
		// flow's cookie-setting redirect is meaningless to a script).
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		// 3. Token via query (?token=) or login form (POST token=).
		fromQuery := r.URL.Query().Get("token")
		provided := fromQuery
		if provided == "" {
			provided = r.PostFormValue("token")
		}
		if provided != "" {
			if !ok(provided) {
				s.renderLogin(w, sanitizeNext(r), true)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookie,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			var dest string
			if fromQuery != "" {
				// Strip the token from the URL but keep other params (status).
				q := r.URL.Query()
				q.Del("token")
				u := *r.URL
				u.RawQuery = q.Encode()
				dest = u.RequestURI()
			} else {
				dest = sanitizeNextValue(r.PostFormValue("next"))
			}
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}

		// 4. Unauthenticated — show the login form.
		s.renderLogin(w, sanitizeNext(r), false)
	})
}

// renderLogin writes the token login page. next is the post-login destination
// (carried through a hidden field); showError marks a rejected token.
func (s *Server) renderLogin(w http.ResponseWriter, next string, showError bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	data := struct {
		Next  string
		Error bool
	}{Next: next, Error: showError}
	if err := s.tmpl.ExecuteTemplate(w, "login", data); err != nil {
		log.Printf("webui: render login: %s", err)
	}
}

// sanitizeNext derives a safe same-site post-login redirect target from the
// request. Only GET targets are preserved (POST actions like /approve are not
// meaningful to redirect to); anything that isn't a local path falls back to
// /requests, preventing open redirects.
func sanitizeNext(r *http.Request) string {
	if r.Method != http.MethodGet {
		return "/requests"
	}
	return sanitizeNextValue(r.URL.RequestURI())
}

// sanitizeNextValue validates a submitted redirect target, falling back to
// /requests for anything that is not a local path (open-redirect guard).
func sanitizeNextValue(n string) string {
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") {
		return "/requests"
	}
	return n
}

// LoadToken reads and trims a token file. Returns an error if the file is
// unreadable or empty.
func LoadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading web auth token file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("web auth token file %s is empty", path)
	}
	return token, nil
}
