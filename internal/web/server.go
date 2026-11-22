// Package web serves the dashboard and account management UI.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"
	"github.com/Dukko/ai-usage-tracker/internal/poller"
	"github.com/Dukko/ai-usage-tracker/internal/providers"
	"github.com/Dukko/ai-usage-tracker/internal/push"
	"github.com/Dukko/ai-usage-tracker/internal/secrets"
	"github.com/Dukko/ai-usage-tracker/internal/store"
)

//go:embed templates static
var assets embed.FS

type pending struct {
	providers.PendingAuth
	Provider string
	Name     string
	Created  time.Time
	Device   *providers.DeviceStart // set for device-code sign-ins
}

// Config holds start-up options.
type Config struct {
	AdminUser     string // username for the admin created from AdminPassword
	AdminPassword string // TRACKER_PASSWORD: creates the first admin if there are no users
	TrustProxy    bool   // trust CF-Connecting-IP / X-Forwarded-* headers
}

type Server struct {
	st         *store.Store
	box        *secrets.Box
	pl         *poller.Poller
	publicURL  string
	trustProxy bool
	pages      map[string]*template.Template
	lim        *limiter
	set        atomic.Pointer[store.Settings]

	setupMu   sync.Mutex
	setupCode string

	mu      sync.Mutex
	pending map[string]pending
}

var pageNames = []string{"dashboard", "history", "accounts", "new", "oauth", "account", "push", "device", "settings", "login"}

// assetVersion fingerprints the embedded static files so a CSS or JS change
// gets a new URL. Browsers and CDNs (Cloudflare) can then cache them forever
// instead of serving an old stylesheet after a deploy.
var assetVersion = func() string {
	h := sha256.New()
	for _, name := range []string{"static/style.css", "static/app.js", "static/theme.js"} {
		b, err := fs.ReadFile(assets, name)
		if err != nil {
			return "1"
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)[:4])
}()

func New(st *store.Store, box *secrets.Box, pl *poller.Poller, cfg Config) (http.Handler, error) {
	s := &Server{st: st, box: box, pl: pl, pages: map[string]*template.Template{}, pending: map[string]pending{},
		publicURL: strings.TrimRight(os.Getenv("TRACKER_PUBLIC_URL"), "/"), trustProxy: cfg.TrustProxy, lim: newLimiter()}
	set, err := st.GetSettings()
	if err != nil {
		return nil, err
	}
	s.set.Store(&set)
	if err := s.bootstrap(cfg.AdminUser, cfg.AdminPassword); err != nil {
		return nil, err
	}
	for _, page := range pageNames {
		t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/base.html", "templates/parts.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		s.pages[page] = t
	}

	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	css := themeCSS()
	admin := s.adminOnly
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /themes.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write([]byte(css))
	})
	icon, err := fs.ReadFile(static, "favicon-32.png")
	if err != nil {
		return nil, err
	}
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png") // every browser accepts a PNG here
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(icon)
	})
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /setup", s.setupForm)
	mux.HandleFunc("POST /setup", s.setup)
	mux.HandleFunc("POST /logout", s.logout)

	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /history", s.history)
	mux.HandleFunc("GET /accounts", s.accountsList)
	mux.HandleFunc("GET /accounts/new", admin(s.newAccount))
	mux.HandleFunc("POST /accounts/apikey", admin(s.createAPIKey))
	mux.HandleFunc("POST /accounts/import", admin(s.importCreds))
	mux.HandleFunc("POST /oauth/start", admin(s.oauthStart))
	mux.HandleFunc("POST /oauth/finish", admin(s.oauthFinish))
	mux.HandleFunc("GET /oauth/device/{pending}", admin(s.devicePoll))
	mux.HandleFunc("GET /accounts/{id}", s.accountDetail)
	mux.HandleFunc("POST /accounts/refresh", admin(s.refreshAll))
	mux.HandleFunc("POST /accounts/{id}/poll", admin(s.pollAccount))
	mux.HandleFunc("POST /accounts/{id}/rename", admin(s.renameAccount))
	mux.HandleFunc("POST /accounts/{id}/delete", admin(s.deleteAccount))
	mux.HandleFunc("POST /accounts/push", admin(s.createPush))
	mux.HandleFunc("POST /accounts/{id}/token", admin(s.regenerateToken))

	mux.HandleFunc("POST /theme/next", s.cycleTheme)
	mux.HandleFunc("GET /settings", s.settingsPage)
	mux.HandleFunc("POST /settings/appearance", s.saveAppearance)
	mux.HandleFunc("POST /settings/dashboard", s.saveDashboard)
	mux.HandleFunc("POST /settings/password", s.changePassword)
	mux.HandleFunc("POST /settings/sessions/others", s.signOutOthers)
	mux.HandleFunc("POST /settings/checks", admin(s.saveChecks))
	mux.HandleFunc("POST /settings/users", admin(s.addUser))
	mux.HandleFunc("POST /settings/users/{id}/password", admin(s.resetUserPassword))
	mux.HandleFunc("POST /settings/users/{id}/delete", admin(s.deleteUser))
	mux.HandleFunc("POST /settings/tokens", admin(s.createAPIToken))
	mux.HandleFunc("POST /settings/tokens/{id}/delete", admin(s.deleteAPIToken))

	mux.HandleFunc("GET /api/accounts", s.apiAccounts)
	mux.HandleFunc("POST /api/ingest", s.ingest)

	go s.pruneSessions()
	return securityHeaders(http.NewCrossOriginProtection().Handler(s.authenticate(mux))), nil
}

func (s *Server) settings() store.Settings { return *s.set.Load() }

func (s *Server) pruneSessions() {
	for {
		_ = s.st.PruneSessions()
		time.Sleep(time.Hour)
	}
}

// securityHeaders keeps the UI out of frames and limits what pages can load.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
			"font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if r.URL.Path != "/healthz" && !strings.HasPrefix(r.URL.Path, "/static/") && r.URL.Path != "/themes.css" && r.URL.Path != "/favicon.ico" {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// layout is what base.html renders around every page.
type layout struct {
	Title        string
	Nav          string
	User         *store.User
	Theme        string
	ThemeLabel   string
	LightTheme   string
	DarkTheme    string
	Follow       bool
	Compact      bool
	AssetVersion string
	Flash        string
	FlashErr     string
	Data         any
}

var flashes = map[string]string{
	"refreshing": "Checking every account now. Reload in a moment to see new numbers.",
	"saved":      "Saved.",
	"password":   "Password updated.",
	"signedout":  "Signed out of your other sessions.",
	"renamed":    "Renamed.",
	"user-added": "User added.",
	"user-reset": "Password reset. Share the new one with them.",
	"user-gone":  "User removed.",
	"revoked":    "Token revoked.",
	"deleted":    "Account deleted.",
}

func (s *Server) newLayout(r *http.Request, title, nav string, data any) layout {
	l := layout{Title: title, Nav: nav, Data: data, AssetVersion: assetVersion}
	prefs := store.Prefs{Theme: defaultTheme, FollowSystem: true}
	if id, ok := currentUser(r); ok {
		u := id.User
		l.User = &u
		prefs = u.Prefs
	}
	t := themeOrDefault(prefs.Theme)
	l.Theme, l.ThemeLabel, l.Compact, l.Follow = t.ID, t.Label, prefs.Compact, prefs.FollowSystem
	if l.Follow {
		light, dark := systemPair(t.ID)
		l.LightTheme, l.DarkTheme = light.ID, dark.ID
	}
	if r.URL.Query().Has("ok") {
		l.Flash = flashes[r.URL.Query().Get("ok")]
	}
	return l
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, title, nav string, data any) {
	s.renderLayout(w, page, s.newLayout(r, title, nav, data))
}

func (s *Server) renderLayout(w http.ResponseWriter, page string, l layout) {
	var buf strings.Builder
	if err := s.pages[page].ExecuteTemplate(&buf, "base", l); err != nil {
		slog.Error("render", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(buf.String()))
}

// renderBare renders a page without the signed-in header (sign-in, setup).
func (s *Server) renderBare(w http.ResponseWriter, r *http.Request, page string, data any) {
	l := s.newLayout(r, "Sign in", "", data)
	l.User = nil
	s.renderLayout(w, page, l)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func me(r *http.Request) store.User {
	id, _ := currentUser(r)
	return id.User
}

// ---- dashboard ----

type option struct {
	Label  string
	URL    string
	Active bool
}

type dashboardPage struct {
	Total, Hidden int
	Subs, API     []accountView
	APIFirst      bool
	ShowSubs      bool
	ShowAPI       bool
	Filters       []option
	Ranges        []option
	RangeDays     int
	RangeLabel    string
	Spend         float64
	SpendAccounts int
	Tokens        int64
	Closest       *closest
	Attention     int
	Interval      string
	LastPoll      int64
	CanEdit       bool
}

type closest struct {
	Pct     float64
	Level   string
	Account string
	Window  string
	Reset   string
}

func rangeOf(r *http.Request, prefs store.Prefs) int {
	switch r.URL.Query().Get("range") {
	case "7":
		return 7
	case "30":
		return 30
	case "90":
		return 90
	}
	if prefs.Range == 7 || prefs.Range == 90 {
		return prefs.Range
	}
	return 30
}

func everyLabel(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		if d == time.Hour {
			return "every hour"
		}
		return fmt.Sprintf("every %d hours", int(d.Hours()))
	}
	return fmt.Sprintf("every %d minutes", int(d.Minutes()))
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	u := me(r)
	accounts, err := s.st.ListAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	days := rangeOf(r, u.Prefs)
	show := r.URL.Query().Get("show")
	if show != "subs" && show != "api" {
		show = "all"
	}
	p := dashboardPage{RangeLabel: fmt.Sprintf("%d days", days), RangeDays: days, Interval: everyLabel(s.pl.Interval()),
		APIFirst: u.Prefs.ShowFirst == "api", ShowSubs: show != "api", ShowAPI: show != "subs", CanEdit: u.IsAdmin()}
	q := func(show string, days int) string { return fmt.Sprintf("/?show=%s&range=%d", show, days) }
	for _, f := range []struct{ id, label string }{{"all", "All"}, {"subs", "Subscriptions"}, {"api", "Pay-as-you-go"}} {
		p.Filters = append(p.Filters, option{f.label, q(f.id, days), f.id == show})
	}
	for _, d := range []int{7, 30, 90} {
		p.Ranges = append(p.Ranges, option{fmt.Sprintf("%dd", d), q(show, d), d == days})
	}
	for _, a := range accounts {
		if u.Prefs.IsHidden(a.ID) {
			p.Hidden++
			continue
		}
		p.Total++
		v, err := s.buildView(a, viewOpts{days: days})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p.LastPoll = max(p.LastPoll, a.LastPoll)
		if v.NeedsAction {
			p.Attention++
		}
		for _, qv := range v.Quotas {
			if p.Closest == nil || qv.UsedPercent > p.Closest.Pct {
				p.Closest = &closest{Pct: qv.UsedPercent, Level: qv.Level, Account: a.Name, Window: qv.Label, Reset: qv.ResetsIn}
			}
		}
		if v.IsOAuth {
			p.Subs = append(p.Subs, v)
			continue
		}
		p.API = append(p.API, v)
		p.Tokens += v.TokensRange
		if v.AnyCost {
			p.Spend += v.CostRange
			p.SpendAccounts++
		}
	}
	s.render(w, r, "dashboard", "Dashboard", "dashboard", p)
}

// ---- accounts list ----

type accountsPage struct {
	Subs, API, Push []accountView
	Total           int
	CanEdit         bool
}

func (s *Server) accountsList(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.st.ListAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := accountsPage{CanEdit: me(r).IsAdmin()}
	for _, a := range accounts {
		v, err := s.buildView(a, viewOpts{days: 30})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p.Total++
		switch v.Kind {
		case "sub":
			p.Subs = append(p.Subs, v)
		case "push":
			p.Push = append(p.Push, v)
		default:
			p.API = append(p.API, v)
		}
	}
	s.render(w, r, "accounts", "Accounts", "accounts", p)
}

// ---- account page ----

type modelRow struct {
	store.ModelTotal
	Share template.CSS
}

type accountPage struct {
	accountView
	CanEdit   bool
	Tiles     []tile
	Chart     detailChart
	Metrics   []option
	Bases     []option
	Models    []modelRow
	Days      []store.DayTotal // newest first
	Interval  string
	Warn      int
	CredLabel string
}

type tile struct {
	Label, Value, Sub string
	Level             string
}

func (s *Server) accountDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a, err := s.st.GetAccount(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	basis := 7
	if r.URL.Query().Get("basis") == "30" {
		basis = 30
	}
	v, err := s.buildView(a, viewOpts{days: 30, detail: true, basis: basis})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := accountPage{accountView: v, CanEdit: me(r).IsAdmin(), Interval: everyLabel(s.pl.Interval()), Warn: s.settings().Warn}
	switch v.Kind {
	case "sub":
		p.CredLabel = "Subscription sign-in (OAuth)"
	case "push":
		p.CredLabel = "Push token, sent from your machine"
	default:
		p.CredLabel = "API key"
	}

	if v.IsOAuth {
		top := 0.0
		var topLabel string
		for _, q := range v.Quotas {
			if q.UsedPercent >= top {
				top, topLabel = q.UsedPercent, q.Label
			}
		}
		if len(v.Quotas) > 0 {
			p.Tiles = append(p.Tiles, tile{"Highest limit now", fmt.Sprintf("%.0f%%", top), topLabel, s.level(top)})
		}
		if a.Plan != "" {
			p.Tiles = append(p.Tiles, tile{"Plan", upperFirst(a.Plan), v.ProviderLabel, ""})
		}
		p.Tiles = append(p.Tiles, tile{"Limit windows", strconv.Itoa(len(v.Quotas)), "tracked", ""},
			tile{"Last check", ago(a.LastPoll), p.Interval, ""})
		s.render(w, r, "account", a.Name, "accounts", p)
		return
	}

	days, err := s.st.DailyTotals(a.ID, 30)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var tok30, req30 int64
	for _, d := range days {
		tok30 += d.Tokens()
		req30 += d.Requests
	}
	for i := len(days) - 1; i >= 0; i-- {
		p.Days = append(p.Days, days[i])
	}
	metric := "tokens"
	if v.AnyCost && r.URL.Query().Get("metric") != "tokens" {
		metric = "spend"
	}
	p.Chart = buildDetailChart(days, metric, v.Forecast)
	link := func(metric string, basis int) string {
		return fmt.Sprintf("/accounts/%d?metric=%s&basis=%d#chart", a.ID, metric, basis)
	}
	if v.AnyCost {
		p.Metrics = []option{{"Spend", link("spend", basis), metric == "spend"}, {"Tokens", link("tokens", basis), metric == "tokens"}}
		p.Bases = []option{{"7 days", link(metric, 7) + "", basis == 7}, {"30 days", link(metric, 30), basis == 30}}
		p.Tiles = append(p.Tiles, tile{"Today", money(v.CostToday), "UTC day so far", ""}, tile{"Last 30 days", money(v.CostRange), "spend", ""})
	}
	p.Tiles = append(p.Tiles, tile{"Tokens · 30 days", tokens(tok30), fmt.Sprintf("%s requests", tokens(req30)), ""})
	if a.Balance != nil {
		sub := "credits left"
		if f := v.Forecast; f != nil && f.HasBalance {
			sub = "lasts to ~" + f.BalanceDate + " at this pace"
		}
		p.Tiles = append(p.Tiles, tile{"Balance", money(*a.Balance), sub, ""})
	}
	if v.IsPush {
		p.Tiles = append(p.Tiles, tile{"Last push", ago(a.LastPoll), "from your machine", ""})
	}

	models, err := s.st.ModelTotals(a.ID, 30)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var maxTok int64
	for _, m := range models {
		maxTok = max(maxTok, m.Tokens())
	}
	for _, m := range models {
		share := 0.0
		if maxTok > 0 {
			share = float64(m.Tokens()) / float64(maxTok) * 100
		}
		p.Models = append(p.Models, modelRow{m, template.CSS(fmt.Sprintf("width:%.1f%%", share))})
	}
	s.render(w, r, "account", a.Name, "accounts", p)
}

type newPage struct {
	APIProviders    []providers.APIKeyProvider
	OAuthProviders  []providers.OAuthProvider // paste-code sign-in + CLI credential import
	SignInProviders []providers.Provider      // everything offered under "Connect"
	PushProviders   []providers.PushProvider
	Error           string
}

func (s *Server) newPageData(errMsg string) newPage {
	p := newPage{Error: errMsg}
	for _, prov := range providers.All() {
		// A provider can offer several ways in (e.g. OpenCode: device sign-in or API key).
		if x, ok := prov.(providers.OAuthProvider); ok {
			p.OAuthProviders = append(p.OAuthProviders, x)
			p.SignInProviders = append(p.SignInProviders, prov)
		}
		if _, ok := prov.(providers.DeviceProvider); ok {
			p.SignInProviders = append(p.SignInProviders, prov)
		}
		if x, ok := prov.(providers.PushProvider); ok {
			p.PushProviders = append(p.PushProviders, x)
		}
		if x, ok := prov.(providers.APIKeyProvider); ok {
			p.APIProviders = append(p.APIProviders, x)
		}
	}
	return p
}

func (s *Server) newAccount(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "new", "Add account", "accounts", s.newPageData(""))
}

func (s *Server) newAccountError(w http.ResponseWriter, r *http.Request, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, r, "new", "Add account", "accounts", s.newPageData(msg))
}

// ---- account creation ----

func (s *Server) saveAccount(ctx context.Context, name string, prov providers.Provider, c *core.Credentials) (int64, error) {
	if name = strings.TrimSpace(name); name == "" {
		name = prov.Label()
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return 0, err
	}
	sealed, err := s.box.Seal(plain)
	if err != nil {
		return 0, err
	}
	id, err := s.st.CreateAccount(name, prov.ID(), sealed)
	if err != nil {
		return 0, err
	}
	// Poll right away so the dashboard has data (or shows the error) immediately.
	pollCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	_ = s.pl.Poll(pollCtx, id)
	return id, nil
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	prov, ok := providers.Get(r.FormValue("provider"))
	if _, takesKey := prov.(providers.APIKeyProvider); !ok || !takesKey {
		s.newAccountError(w, r, "Unknown API provider.")
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		s.newAccountError(w, r, "API key is required.")
		return
	}
	id, err := s.saveAccount(r.Context(), r.FormValue("name"), prov, &core.Credentials{APIKey: key})
	if err != nil {
		s.newAccountError(w, r, err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/accounts/%d", id), http.StatusSeeOther)
}

func (s *Server) importCreds(w http.ResponseWriter, r *http.Request) {
	prov, ok := providers.Get(r.FormValue("provider"))
	op, isOAuth := prov.(providers.OAuthProvider)
	if !ok || !isOAuth {
		s.newAccountError(w, r, "Unknown subscription provider.")
		return
	}
	c, err := op.ImportJSON([]byte(r.FormValue("json")))
	if err != nil {
		s.newAccountError(w, r, "Could not read credentials: "+err.Error())
		return
	}
	id, err := s.saveAccount(r.Context(), r.FormValue("name"), prov, c)
	if err != nil {
		s.newAccountError(w, r, err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/accounts/%d", id), http.StatusSeeOther)
}

type oauthPage struct {
	PendingID    string
	Label        string
	AuthURL      string
	Instructions string
	Error        string
}

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	prov, ok := providers.Get(r.FormValue("provider"))
	if dp, isDevice := prov.(providers.DeviceProvider); ok && isDevice {
		s.deviceStart(w, r, dp)
		return
	}
	op, isOAuth := prov.(providers.OAuthProvider)
	if !ok || !isOAuth {
		s.newAccountError(w, r, "Unknown subscription provider.")
		return
	}
	pa := providers.NewPendingAuth()
	s.mu.Lock()
	for k, v := range s.pending {
		if time.Since(v.Created) > 30*time.Minute {
			delete(s.pending, k)
		}
	}
	s.pending[pa.State] = pending{PendingAuth: pa, Provider: prov.ID(), Name: r.FormValue("name"), Created: time.Now()}
	s.mu.Unlock()

	s.render(w, r, "oauth", "Connect "+prov.Label(), "accounts", oauthPage{PendingID: pa.State, Label: prov.Label(), AuthURL: op.AuthURL(pa), Instructions: op.Instructions()})
}

func (s *Server) oauthFinish(w http.ResponseWriter, r *http.Request) {
	pid := r.FormValue("pending")
	s.mu.Lock()
	p, ok := s.pending[pid]
	s.mu.Unlock()
	if !ok {
		s.newAccountError(w, r, "That login attempt expired. Please start again.")
		return
	}
	prov, _ := providers.Get(p.Provider)
	op := prov.(providers.OAuthProvider)

	c, err := op.Exchange(r.Context(), r.FormValue("code"), p.PendingAuth)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "oauth", "Connect "+prov.Label(), "accounts", oauthPage{PendingID: pid, Label: prov.Label(), AuthURL: op.AuthURL(p.PendingAuth),
			Instructions: op.Instructions(), Error: err.Error()})
		return
	}
	s.mu.Lock()
	delete(s.pending, pid)
	s.mu.Unlock()

	id, err := s.saveAccount(r.Context(), p.Name, prov, c)
	if err != nil {
		s.newAccountError(w, r, err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/accounts/%d", id), http.StatusSeeOther)
}

// ---- account actions ----

func (s *Server) pollAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	_ = s.pl.Poll(ctx, id) // errors are recorded on the account and shown in the UI
	http.Redirect(w, r, safeNext(r.FormValue("back")), http.StatusSeeOther)
}

func (s *Server) refreshAll(w http.ResponseWriter, r *http.Request) {
	go s.pl.PollAll(context.WithoutCancel(r.Context()))
	http.Redirect(w, r, "/?ok=refreshing", http.StatusSeeOther)
}

func (s *Server) renameAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	name := strings.TrimSpace(r.FormValue("name"))
	if !ok || name == "" || len(name) > 100 {
		http.Error(w, "Names are 1 to 100 characters.", http.StatusBadRequest)
		return
	}
	if err := s.st.RenameAccount(id, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/accounts/%d?ok=renamed", id), http.StatusSeeOther)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.st.DeleteAccount(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/accounts?ok=deleted", http.StatusSeeOther)
}

// ---- JSON API ----

type apiQuota struct {
	Window      string  `json:"window"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at,omitempty"`
	Detail      string  `json:"detail,omitempty"`
}

type apiAccount struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Provider  string     `json:"provider"`
	Plan      string     `json:"plan,omitempty"`
	LastPoll  int64      `json:"last_poll"`
	LastError string     `json:"last_error,omitempty"`
	Quotas    []apiQuota `json:"quotas,omitempty"`
	CostToday *float64   `json:"cost_today_usd,omitempty"`
	Cost30d   *float64   `json:"cost_30d_usd,omitempty"`
	Tokens30d *int64     `json:"tokens_30d,omitempty"`
	Balance   *float64   `json:"balance_usd,omitempty"`
}

func (s *Server) apiAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.st.ListAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]apiAccount, 0, len(accounts))
	for _, a := range accounts {
		v, err := s.buildView(a, viewOpts{days: 30})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		item := apiAccount{ID: a.ID, Name: a.Name, Provider: a.Provider, Plan: a.Plan, LastPoll: a.LastPoll,
			LastError: a.LastError, Balance: a.Balance}
		for _, q := range v.Quotas {
			item.Quotas = append(item.Quotas, apiQuota{Window: q.Window, UsedPercent: q.UsedPercent, ResetsAt: q.ResetsAt, Detail: q.Detail})
		}
		if !v.IsOAuth {
			item.CostToday, item.Cost30d, item.Tokens30d = &v.CostToday, &v.CostRange, &v.TokensRange
		}
		out = append(out, item)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ---- push accounts ----

type pushPage struct {
	ID      int64
	Name    string
	Label   string
	Command string
	Token   string
	URL     string
}

// newPushToken returns a token of the form "<account id>.<secret>" and the
// credentials holding the secret's hash. Only the hash is stored.
func newPushToken(id int64) (string, *core.Credentials, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	secret := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(secret))
	return fmt.Sprintf("%d.%s", id, secret), &core.Credentials{TokenHash: hex.EncodeToString(sum[:])}, nil
}

func (s *Server) baseURL(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// setPushToken issues a fresh token for the account and renders the setup page.
func (s *Server) setPushToken(w http.ResponseWriter, r *http.Request, a store.Account, prov providers.PushProvider) {
	token, creds, err := newPushToken(a.ID)
	if err == nil {
		var plain, sealed []byte
		if plain, err = json.Marshal(creds); err == nil {
			if sealed, err = s.box.Seal(plain); err == nil {
				err = s.st.UpdateSecret(a.ID, sealed)
			}
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "push", a.Name+" push token", "accounts", pushPage{ID: a.ID, Name: a.Name, Label: prov.Label(), Command: prov.PushCommand(), Token: token, URL: s.baseURL(r)})
}

func (s *Server) createPush(w http.ResponseWriter, r *http.Request) {
	prov, ok := providers.Get(r.FormValue("provider"))
	pp, isPush := prov.(providers.PushProvider)
	if !ok || !isPush {
		s.newAccountError(w, r, "Unknown local tool.")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = prov.Label()
	}
	// Placeholder secret; replaced with the real token hash once the id is known.
	id, err := s.st.CreateAccount(name, prov.ID(), []byte{})
	if err != nil {
		s.newAccountError(w, r, err.Error())
		return
	}
	a, err := s.st.GetAccount(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.setPushToken(w, r, a, pp)
}

func (s *Server) regenerateToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a, err := s.st.GetAccount(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	prov, _ := providers.Get(a.Provider)
	pp, isPush := prov.(providers.PushProvider)
	if !isPush {
		http.Error(w, "not a push account", http.StatusBadRequest)
		return
	}
	s.setPushToken(w, r, a, pp)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ingest accepts usage from push clients. Auth: "Authorization: Bearer <id>.<secret>".
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	idStr, secret, ok2 := strings.Cut(token, ".")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if !ok || !ok2 || err != nil || secret == "" {
		jsonError(w, http.StatusUnauthorized, "missing or malformed token")
		return
	}
	a, err := s.st.GetAccount(id)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if prov, ok := providers.Get(a.Provider); !ok || prov.Kind() != providers.KindPush {
		jsonError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	var creds core.Credentials
	plain, err := s.box.Open(a.Secret)
	if err == nil {
		err = json.Unmarshal(plain, &creds)
	}
	sum := sha256.Sum256([]byte(secret))
	if err != nil || creds.TokenHash == "" ||
		subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(creds.TokenHash)) != 1 {
		jsonError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	var p push.Payload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&p); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(p.Rows) > 100_000 {
		jsonError(w, http.StatusRequestEntityTooLarge, "too many rows")
		return
	}
	res := &core.Result{}
	costs := map[string]float64{}
	for i, row := range p.Rows {
		if _, err := time.Parse(time.DateOnly, row.Day); err != nil {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("row %d: bad day %q", i, row.Day))
			return
		}
		if row.Model == "" || len(row.Model) > 300 || row.Input < 0 || row.Output < 0 ||
			row.CacheRead < 0 || row.CacheWrite < 0 || row.Requests < 0 || row.CostUSD < 0 {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("row %d: invalid values", i))
			return
		}
		res.Usage = append(res.Usage, core.DailyUsage{Day: row.Day, Model: row.Model, Input: row.Input, Output: row.Output,
			CacheRead: row.CacheRead, CacheWrite: row.CacheWrite, Requests: row.Requests})
		costs[row.Day] += row.CostUSD
	}
	for day, usd := range costs {
		res.Costs = append(res.Costs, core.DailyCost{Day: day, USD: usd})
	}
	if err := s.st.SaveResult(a.ID, res); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "rows": len(p.Rows)})
}

// ---- device-code sign-in ----

type devicePage struct {
	PendingID string
	Label     string
	UserCode  string
	VerifyURL string
	Refresh   int
	Error     string
}

func (s *Server) addPending(p pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.pending {
		if time.Since(v.Created) > 30*time.Minute {
			delete(s.pending, k)
		}
	}
	s.pending[p.State] = p
}

func (s *Server) deviceStart(w http.ResponseWriter, r *http.Request, dp providers.DeviceProvider) {
	start, err := dp.StartDevice(r.Context())
	if err != nil {
		s.newAccountError(w, r, "Could not start sign-in: "+err.Error())
		return
	}
	pa := providers.NewPendingAuth()
	s.addPending(pending{PendingAuth: pa, Provider: dp.ID(), Name: r.FormValue("name"), Created: time.Now(), Device: start})
	s.render(w, r, "device", "Connect "+dp.Label(), "accounts", devicePage{PendingID: pa.State, Label: dp.Label(), UserCode: start.UserCode,
		VerifyURL: start.VerifyURL, Refresh: start.Interval})
}

// devicePoll is hit by the waiting page's meta refresh; each visit polls the token endpoint once.
func (s *Server) devicePoll(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pending")
	s.mu.Lock()
	p, ok := s.pending[pid]
	s.mu.Unlock()
	if !ok || p.Device == nil || time.Since(p.Created) > time.Duration(p.Device.ExpiresIn)*time.Second {
		s.newAccountError(w, r, "That sign-in expired. Please start again.")
		return
	}
	prov, _ := providers.Get(p.Provider)
	dp := prov.(providers.DeviceProvider)
	page := devicePage{PendingID: pid, Label: prov.Label(), UserCode: p.Device.UserCode, VerifyURL: p.Device.VerifyURL, Refresh: p.Device.Interval}

	creds, label, err := dp.PollDevice(r.Context(), p.Device.DeviceCode)
	if errors.Is(err, providers.ErrAuthPending) {
		s.render(w, r, "device", "Connect "+prov.Label(), "accounts", page)
		return
	}
	s.mu.Lock()
	delete(s.pending, pid)
	s.mu.Unlock()
	if err != nil {
		s.newAccountError(w, r, err.Error())
		return
	}
	name := p.Name
	if strings.TrimSpace(name) == "" && label != "" {
		name = prov.Label() + " (" + label + ")"
	}
	id, err := s.saveAccount(r.Context(), name, prov, creds)
	if err != nil {
		s.newAccountError(w, r, err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/accounts/%d", id), http.StatusSeeOther)
}
