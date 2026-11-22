package web

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"
	"github.com/Dukko/ai-usage-tracker/internal/poller"
	"github.com/Dukko/ai-usage-tracker/internal/secrets"
	"github.com/Dukko/ai-usage-tracker/internal/store"
)

type env struct {
	t  *testing.T
	st *store.Store
	h  http.Handler
}

func newEnv(t *testing.T, cfg Config) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secrets.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(st, box, poller.New(st, box, 10*time.Minute), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, st: st, h: h}
}

// do sends a request as if from the given IP, with an optional session cookie.
func (e *env) do(method, path, ip, cookie string, form url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = ip + ":1234"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) login(user, pw, ip string) string {
	e.t.Helper()
	w := e.do("POST", "/login", ip, "", url.Values{"username": {user}, "password": {pw}})
	if w.Code != http.StatusSeeOther {
		e.t.Fatalf("login %s: status %d", user, w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	e.t.Fatal("no session cookie")
	return ""
}

func (e *env) seed() (sub, api, push int64) {
	e.t.Helper()
	must := func(err error) {
		if err != nil {
			e.t.Helper()
			e.t.Fatal(err)
		}
	}
	var err error
	sub, err = e.st.CreateAccount("Claude Max", "claude-sub", []byte("x"))
	must(err)
	api, err = e.st.CreateAccount("OpenRouter", "openrouter", []byte("x"))
	must(err)
	push, err = e.st.CreateAccount("OpenCode laptop", "opencode", []byte("x"))
	must(err)
	reset := time.Now().Add(2 * time.Hour).Unix()
	must(e.st.SaveResult(sub, &core.Result{Plan: "max", Quotas: []core.Quota{{Window: "5h", UsedPercent: 82, ResetsAt: reset}, {Window: "7d", UsedPercent: 41}}}))
	bal := 61.8
	res := &core.Result{Balance: &bal, Quotas: []core.Quota{{Window: "key limit (monthly)", UsedPercent: 62, Detail: "$62.10 of $100.00",
		ResetsAt: time.Now().AddDate(0, 0, 17).Unix()}}}
	for i := range 30 {
		day := time.Now().UTC().AddDate(0, 0, -i).Format(time.DateOnly)
		res.Costs = append(res.Costs, core.DailyCost{Day: day, USD: 1.5 + float64(i%5)})
		res.Usage = append(res.Usage, core.DailyUsage{Day: day, Model: "anthropic/claude-sonnet", Input: 1000, Output: 300, Requests: 4})
	}
	must(e.st.SaveResult(api, res))
	must(e.st.SaveResult(push, &core.Result{Usage: []core.DailyUsage{{Day: time.Now().UTC().Format(time.DateOnly), Model: "m", Input: 5}}}))
	must(e.st.RecordError(push, "token expired"))
	return
}

var templateErr = regexp.MustCompile(`ZgotmplZ|template error|<no value>`)

func TestPagesRender(t *testing.T) {
	e := newEnv(t, Config{AdminPassword: "correct horse"})
	sub, api, push := e.seed()
	c := e.login("admin", "correct horse", "10.0.0.1")
	for _, path := range []string{"/", "/history", "/history?range=90&metric=tokens", fmt.Sprintf("/history?range=365&account=%d", api),
		fmt.Sprintf("/history?range=30&account=%d", sub),
		"/?show=api&range=90", "/?show=subs&range=7", "/accounts", "/accounts/new", "/settings",
		fmt.Sprintf("/accounts/%d", sub), fmt.Sprintf("/accounts/%d", api), fmt.Sprintf("/accounts/%d?metric=tokens&basis=30", api),
		fmt.Sprintf("/accounts/%d", push)} {
		w := e.do("GET", path, "10.0.0.1", c, nil)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		if m := templateErr.FindString(w.Body.String()); m != "" {
			t.Errorf("GET %s: found %q in output", path, m)
		}
	}
	body := e.do("GET", fmt.Sprintf("/accounts/%d", api), "10.0.0.1", c, nil).Body.String()
	for _, want := range []string{"Balance runs out", "Burn rate", "Limit reached", "By model"} {
		if !strings.Contains(body, want) {
			t.Errorf("account page missing %q", want)
		}
	}
	history := e.do("GET", "/history", "10.0.0.1", c, nil).Body.String()
	for _, want := range []string{"By account", "By model", "Daily breakdown", "OpenRouter"} {
		if !strings.Contains(history, want) {
			t.Errorf("history page missing %q", want)
		}
	}
	accounts := e.do("GET", "/accounts", "10.0.0.1", c, nil).Body.String()
	for _, want := range []string{"Subscriptions", "Pay-as-you-go", "Local tools"} {
		if !strings.Contains(accounts, want) {
			t.Errorf("accounts page missing %q", want)
		}
	}
	if !strings.Contains(e.do("GET", "/", "10.0.0.1", c, nil).Body.String(), "At this pace") {
		t.Error("dashboard missing forecast note")
	}
}

func TestAuthRequiredAndHeaders(t *testing.T) {
	e := newEnv(t, Config{AdminPassword: "correct horse"})
	w := e.do("GET", "/settings", "10.0.0.1", "", nil)
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Fatalf("unauthenticated: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("missing framing protection")
	}
	if w := e.do("GET", "/api/accounts", "10.0.0.1", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("api without auth: %d", w.Code)
	}
	if w := e.do("GET", "/favicon.ico", "10.0.0.1", "", nil); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Errorf("favicon: %d", w.Code)
	}
	if w := e.do("GET", "/static/favicon.svg", "10.0.0.1", "", nil); w.Code != 200 {
		t.Errorf("svg favicon: %d", w.Code)
	}
	if w := e.do("GET", "/login", "10.0.0.1", "", nil); w.Code != 200 {
		t.Errorf("login page: %d", w.Code)
	}
	if w := e.do("GET", "/accounts", "10.0.0.1", "forged", nil); w.Code != http.StatusSeeOther {
		t.Errorf("forged cookie: %d", w.Code)
	}
}

func TestLockout(t *testing.T) {
	e := newEnv(t, Config{AdminPassword: "correct horse"})
	for i := range maxFailures {
		w := e.do("POST", "/login", "10.0.0.9", "", url.Values{"username": {"admin"}, "password": {"nope"}})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := e.do("POST", "/login", "10.0.0.9", "", url.Values{"username": {"admin"}, "password": {"correct horse"}})
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "Sign-in paused") {
		t.Fatalf("expected lockout, got %d", w.Code)
	}
	// Another address is unaffected.
	e.login("admin", "correct horse", "10.0.0.10")
}

func TestViewerIsReadOnly(t *testing.T) {
	e := newEnv(t, Config{AdminPassword: "correct horse"})
	_, api, _ := e.seed()
	admin := e.login("admin", "correct horse", "10.0.0.1")
	w := e.do("POST", "/settings/users", "10.0.0.1", admin, url.Values{"username": {"viewer"}, "password": {"viewer-pass"}, "role": {"viewer"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("add user: %d %s", w.Code, w.Body.String())
	}
	v := e.login("viewer", "viewer-pass", "10.0.0.2")
	for _, path := range []string{fmt.Sprintf("/accounts/%d/delete", api), fmt.Sprintf("/accounts/%d/poll", api), "/settings/checks", "/settings/tokens", "/accounts/refresh"} {
		if w := e.do("POST", path, "10.0.0.2", v, url.Values{}); w.Code != http.StatusForbidden {
			t.Errorf("viewer POST %s: %d", path, w.Code)
		}
	}
	if w := e.do("GET", "/accounts/new", "10.0.0.2", v, nil); w.Code != http.StatusForbidden {
		t.Errorf("viewer add page: %d", w.Code)
	}
	if w := e.do("GET", "/", "10.0.0.2", v, nil); w.Code != 200 || strings.Contains(w.Body.String(), "Refresh all") {
		t.Errorf("viewer dashboard: %d", w.Code)
	}
	if w := e.do("GET", fmt.Sprintf("/accounts/%d", api), "10.0.0.2", v, nil); w.Code != 200 ||
		!strings.Contains(w.Body.String(), "History") || strings.Contains(w.Body.String(), "Delete") {
		t.Errorf("viewer account page: %d", w.Code)
	}
	// Viewers can still change their own appearance.
	if w := e.do("POST", "/settings/appearance", "10.0.0.2", v, url.Values{"theme": {"gruvbox"}}); w.Code != http.StatusSeeOther {
		t.Errorf("viewer appearance: %d", w.Code)
	}
	if u, _ := e.st.GetUserByName("viewer"); u.Prefs.Theme != "gruvbox" {
		t.Errorf("theme not saved: %+v", u.Prefs)
	}
}

func TestAPITokenAndLogout(t *testing.T) {
	e := newEnv(t, Config{AdminPassword: "correct horse"})
	e.seed()
	c := e.login("admin", "correct horse", "10.0.0.1")
	w := e.do("POST", "/settings/tokens", "10.0.0.1", c, url.Values{"name": {"Home Assistant"}})
	tok := regexp.MustCompile(`aut_[A-Za-z0-9_-]+`).FindString(w.Body.String())
	if tok == "" {
		t.Fatal("token not shown")
	}
	r := httptest.NewRequest("GET", "/api/accounts", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"OpenRouter"`) {
		t.Fatalf("api with token: %d %s", rec.Code, rec.Body.String())
	}
	// The read-only token can't reach the HTML UI.
	r = httptest.NewRequest("GET", "/settings", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("token on HTML page: %d", rec.Code)
	}

	e.do("POST", "/logout", "10.0.0.1", c, url.Values{})
	if w := e.do("GET", "/", "10.0.0.1", c, nil); w.Code != http.StatusSeeOther {
		t.Errorf("session still valid after logout: %d", w.Code)
	}
}

func TestSetupCode(t *testing.T) {
	e := newEnv(t, Config{})
	if w := e.do("GET", "/", "10.0.0.1", "", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect, got %d", w.Code)
	}
	if w := e.do("GET", "/login", "10.0.0.1", "", nil); w.Header().Get("Location") != "/setup" {
		t.Fatalf("login should send to setup, got %q", w.Header().Get("Location"))
	}
	form := url.Values{"code": {"WRONG-CODE"}, "username": {"me"}, "password": {"long enough"}, "confirm": {"long enough"}}
	if w := e.do("POST", "/setup", "10.0.0.1", "", form); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong code accepted: %d", w.Code)
	}
	if n, _ := e.st.CountUsers(); n != 0 {
		t.Fatal("user created with wrong code")
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := hashPassword("secret password")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(h, "secret password") || checkPassword(h, "secret passworD") || checkPassword("garbage", "x") {
		t.Fatal("hash check wrong")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/accounts/3": "/accounts/3", "//evil.com": "/", "https://evil.com": "/", "/\\evil.com": "/", "/login": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q", in, got)
		}
	}
}

func TestQuotaForecastFromOneReading(t *testing.T) {
	lvl := func(p float64) string {
		if p >= 90 {
			return "crit"
		}
		return "ok"
	}
	now := time.Now().Unix()
	// 5-hour window, 2 hours in, 40% used: 20%/h, so 100% in 3h is exactly at the reset; 50% used would hit it early.
	one := []store.QuotaSample{{TS: now, UsedPercent: 50, ResetsAt: now + 3*3600}}
	msg, l := quotaForecast("5h", one, now+3*3600, lvl)
	if !strings.Contains(msg, "hits 100%") || l != "crit" {
		t.Errorf("5h at 50%% after 2h: %q %q", msg, l)
	}
	// Weekly window, 1 day in, 5% used: about 35% by the reset.
	one = []store.QuotaSample{{TS: now, UsedPercent: 5, ResetsAt: now + 6*86400}}
	if msg, _ := quotaForecast("7d opus", one, now+6*86400, lvl); !strings.Contains(msg, "about 35%") {
		t.Errorf("weekly: %q", msg)
	}
	// OpenCode Go names: weekly at 49% with 25h left is ~58% by the reset; monthly at 70% with 10 days left hits 100% first.
	one = []store.QuotaSample{{TS: now, UsedPercent: 49}}
	if msg, _ := quotaForecast("weekly", one, now+25*3600, lvl); !strings.Contains(msg, "about 58%") {
		t.Errorf("opencode weekly: %q", msg)
	}
	one = []store.QuotaSample{{TS: now, UsedPercent: 70}}
	if msg, _ := quotaForecast("monthly", one, now+10*86400, lvl); !strings.Contains(msg, "hits 100%") {
		t.Errorf("opencode monthly: %q", msg)
	}
	// Flat history that doesn't reach back to the window start says nothing.
	flat := []store.QuotaSample{{TS: now - 3600, UsedPercent: 70}, {TS: now, UsedPercent: 70}}
	if msg, _ := quotaForecast("budget", flat, now+86400, lvl); msg != "" {
		t.Errorf("flat partial history: %q", msg)
	}
	// Unknown window length with one reading: no guess.
	if msg, _ := quotaForecast("budget: opus", one, now+6*86400, lvl); msg != "" {
		t.Errorf("unknown window: %q", msg)
	}
}
