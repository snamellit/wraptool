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
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/requestlog"
)

//go:embed templates
var assets embed.FS

// tmplFuncs provides a minimal `dict` helper so a partial template can be
// invoked with several named values (Go's html/template passes a single
// pipeline argument to {{template}}).
var tmplFuncs = template.FuncMap{
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
	mux.HandleFunc("GET /rules", s.handleRules)
	mux.HandleFunc("POST /rules/{tool}/{kind}/add", s.handleRuleAdd)
	mux.HandleFunc("POST /rules/{tool}/{kind}/{index}/update", s.handleRuleUpdate)
	mux.HandleFunc("POST /rules/{tool}/{kind}/{index}/delete", s.handleRuleDelete)
	mux.Handle("GET /static/", s.static)
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

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	apply := r.FormValue("apply") == "1"

	req, err := s.store.Get(id)
	if err != nil {
		s.redirect(w, r, "/requests", "err", err.Error(), "")
		return
	}
	if err := s.store.UpdateStatus(id, requestlog.StatusApproved, ""); err != nil {
		s.redirect(w, r, "/requests", "err", "updating status: "+err.Error(), "")
		return
	}

	if !apply {
		s.redirect(w, r, "/requests", "ok", fmt.Sprintf("Approved %s. Add the capability to config manually; a running wraptool hot-reloads on save.", id), "")
		return
	}

	result, err := config.ApplyRequest(s.configPath, req.Tool, req.Subcommand, req.Flags)
	if err != nil {
		// Status is already approved; report the apply failure so the
		// operator can edit the config by hand.
		s.redirect(w, r, "/requests", "err", fmt.Sprintf("Approved %s but applying to config failed: %s", id, err), "")
		return
	}

	switch {
	case result.NewRule:
		s.redirect(w, r, "/requests", "ok", fmt.Sprintf("Approved %s — added allow rule for %s %s. Server will hot-reload.", id, req.Tool, strings.Join(req.Subcommand, " ")), result.Snippet)
	case len(result.AddedFlags) > 0:
		s.redirect(w, r, "/requests", "ok", fmt.Sprintf("Approved %s — extended rule with flags %s. Server will hot-reload.", id, strings.Join(result.AddedFlags, " ")), result.Snippet)
	default:
		s.redirect(w, r, "/requests", "ok", fmt.Sprintf("Approved %s — capability was already allowed; no config change.", id), "")
	}
}

func (s *Server) handleDeny(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reason := strings.TrimSpace(r.FormValue("reason"))
	if err := s.store.UpdateStatus(id, requestlog.StatusDenied, reason); err != nil {
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
	Kind       string // "allow" | "deny"
	Index      int
	Subcommand string // space-joined tokens
	Flags      string // space-joined tokens
	Advanced   bool   // has flag_constraints / arg_constraints / deny_flags (edit YAML directly)
}

type toolRulesView struct {
	Name   string
	Binary string
	Allow  []ruleView
	Deny   []ruleView
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
		tv := toolRulesView{Name: name, Binary: tc.Binary}
		for i, rule := range tc.Allow {
			tv.Allow = append(tv.Allow, ruleView{
				Kind:       config.KindAllow,
				Index:      i,
				Subcommand: strings.Join(rule.Subcommand, " "),
				Flags:      strings.Join(rule.Flags, " "),
				Advanced:   len(rule.FlagConstraints) > 0 || rule.ArgConstraints != nil || len(rule.DenyFlags) > 0,
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

	if err := config.UpdateRule(s.configPath, tool, kind, index, expect, subcommand, flags); err != nil {
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
	s.redirect(w, r, "/rules", "ok", fmt.Sprintf("Deleted %s rule %s. Server will hot-reload.", kind, tool), "")
}

const sessionCookie = "wraptool_session"

// AuthMiddleware guards next with a shared token. Authentication is accepted,
// in order, from: a valid session cookie; an "Authorization: Bearer <token>"
// header (for scripts); or a `token` value supplied via query param (GET link)
// or login form (POST). On a valid token the middleware sets an HttpOnly
// session cookie and redirects to a clean URL, so subsequent requests carry
// the cookie. When unauthenticated, browsers get a login form; the /static/
// assets are served without auth so that form renders styled. All token
// comparisons are constant-time. If token is empty, next is returned unwrapped
// (localhost-trust mode).
func (s *Server) AuthMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
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
