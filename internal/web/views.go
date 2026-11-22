package web

import (
	"fmt"
	"html/template"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/providers"
	"github.com/Dukko/ai-usage-tracker/internal/store"
)

type quotaView struct {
	Window      string // raw window id, used for history lookups
	Label       string
	UsedPercent float64
	ResetsAt    int64
	ResetsIn    string
	Detail      string
	Level       string
	Fill        template.CSS
	Forecast    string // "At this pace: ..." or ""
	FcLevel     string
	Peak        float64
	Chart       template.HTML // detail page: history with warning line
}

type bar struct {
	Style template.CSS
	Title string
	Class string
}

type accountView struct {
	store.Account
	ProviderLabel string
	Kind          string // "sub", "api" or "push"
	IsOAuth       bool
	IsPush        bool
	Badge         template.CSS
	Initials      string
	Quotas        []quotaView
	Spark         template.HTML
	SparkLabel    string
	RangeDays     int
	CostToday     float64
	CostRange     float64
	TokensRange   int64
	AnyCost       bool
	Bars          []bar
	Forecast      *spendForecast
	NeedsAction   bool
}

// providerStyle gives each service a recognizable badge.
func providerStyle(id string) (color, initials string) {
	switch id {
	case "claude-sub":
		return "#B4532A", "Cl"
	case "anthropic-api":
		return "#8A3B1C", "An"
	case "chatgpt-sub":
		return "#0E7C66", "GP"
	case "openai-api":
		return "#1F6F5C", "OA"
	case "openrouter":
		return "#5B5BD6", "OR"
	case "opencode", "opencode-console":
		return "#2563EB", "OC"
	}
	return "#64748B", strings.ToUpper(id[:min(2, len(id))])
}

var windowWords = map[string]string{"5h": "5-hour", "7d": "Weekly", "1d": "Daily", "24h": "Daily", "30d": "Monthly",
	"daily": "Daily", "weekly": "Weekly", "monthly": "Monthly"}

// windowLabel turns "7d opus" into "Weekly · Opus".
func windowLabel(w string) string {
	head, rest, _ := strings.Cut(w, " ")
	mapped, ok := windowWords[head]
	switch {
	case !ok:
		return upperFirst(w)
	case rest == "":
		return mapped
	default:
		return mapped + " · " + upperFirst(rest)
	}
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Server) level(pct float64) string {
	set := s.settings()
	switch {
	case pct >= float64(set.Crit):
		return "crit"
	case pct >= float64(set.Warn):
		return "warn"
	default:
		return "ok"
	}
}

type viewOpts struct {
	days   int  // chart range
	detail bool // account page: longer history and charts
	basis  int  // forecast averaging window
}

func (s *Server) buildView(a store.Account, o viewOpts) (accountView, error) {
	if o.days <= 0 {
		o.days = 30
	}
	if o.basis <= 0 {
		o.basis = 7
	}
	v := accountView{Account: a, ProviderLabel: a.Provider, Kind: "api", RangeDays: o.days}
	color, initials := providerStyle(a.Provider)
	v.Badge, v.Initials = template.CSS("background:"+color), initials
	if prov, ok := providers.Get(a.Provider); ok {
		v.ProviderLabel = prov.Label()
		switch prov.Kind() {
		case providers.KindOAuth:
			v.IsOAuth, v.Kind = true, "sub"
		case providers.KindPush:
			v.IsPush, v.Kind = true, "push"
		}
	}
	v.NeedsAction = a.LastError != ""

	quotas, err := s.st.LatestQuotas(a.ID)
	if err != nil {
		return v, err
	}
	histDays := 7
	if o.detail {
		histDays = 30
	}
	var sparkFrom int64 = -1
	for _, q := range quotas {
		qv := quotaView{
			Window: q.Window, Label: windowLabel(q.Window), UsedPercent: q.UsedPercent, ResetsAt: q.ResetsAt,
			ResetsIn: until(q.ResetsAt), Detail: q.Detail, Level: s.level(q.UsedPercent),
			Fill: template.CSS(fmt.Sprintf("width:%.1f%%", clamp(q.UsedPercent, 0, 100))),
		}
		if qv.Level == "crit" {
			v.NeedsAction = true
		}
		hist, err := s.st.QuotaHistory(a.ID, q.Window, time.Now().AddDate(0, 0, -histDays))
		if err != nil {
			return v, err
		}
		for _, h := range hist {
			qv.Peak = math.Max(qv.Peak, h.UsedPercent)
		}
		qv.Forecast, qv.FcLevel = quotaForecast(q.Window, hist, q.ResetsAt, s.level)
		if o.detail {
			qv.Chart = s.windowChart(hist, qv.Label)
		} else if q.ResetsAt > sparkFrom && len(hist) > 1 {
			// The card sparkline follows the longest window (furthest reset).
			sparkFrom = q.ResetsAt
			v.Spark = sparkline(downsample(percents(hist), 120), 100)
			v.SparkLabel = qv.Label + " use, last 7 days"
		}
		v.Quotas = append(v.Quotas, qv)
	}
	if v.IsOAuth {
		return v, nil
	}

	need := max(o.days, 30) // 30 days are needed for the forecast and month totals
	days, err := s.st.DailyTotals(a.ID, need)
	if err != nil {
		return v, err
	}
	for _, d := range days {
		v.AnyCost = v.AnyCost || d.USD > 0
	}
	shown := days[len(days)-o.days:]
	for _, d := range shown {
		v.CostRange += d.USD
		v.TokensRange += d.Tokens()
	}
	v.CostToday = days[len(days)-1].USD
	if !o.detail {
		v.Bars = dayBars(shown, v.AnyCost, "")
	}
	if v.AnyCost {
		v.Forecast = s.spendForecast(days, o.basis, a.Balance, v.Quotas)
	}
	return v, nil
}

func percents(h []store.QuotaSample) []float64 {
	out := make([]float64, len(h))
	for i, x := range h {
		out[i] = x.UsedPercent
	}
	return out
}

func clamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }

// ---- forecasts ----

var windowLen = regexp.MustCompile(`^(\d+)([mhd])\b`)

// windowStart works out when the current window began from its id ("5h",
// "7d opus", "weekly", "monthly") and its reset time.
func windowStart(window string, resetsAt int64) (int64, bool) {
	if resetsAt <= 0 {
		return 0, false
	}
	head, _, _ := strings.Cut(strings.ToLower(window), " ")
	reset := time.Unix(resetsAt, 0)
	switch head {
	case "daily":
		return reset.Add(-24 * time.Hour).Unix(), true
	case "weekly":
		return reset.Add(-7 * 24 * time.Hour).Unix(), true
	case "monthly":
		return reset.AddDate(0, -1, 0).Unix(), true
	}
	m := windowLen.FindStringSubmatch(head)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	if n <= 0 {
		return 0, false
	}
	return resetsAt - int64(time.Duration(n)*unit/time.Second), true
}

// quotaForecast extrapolates the average pace since the current window began.
//
// When the window's length is known, its start is reset time minus length and
// usage was 0% then, so a single reading is enough: no history needed. Otherwise
// it falls back to the readings recorded during the current window.
func quotaForecast(window string, hist []store.QuotaSample, resetsAt int64, level func(float64) string) (string, string) {
	if len(hist) == 0 {
		return "", ""
	}
	last := hist[len(hist)-1]
	first := last
	start, known := windowStart(window, resetsAt)
	if known && start < last.TS {
		first = store.QuotaSample{TS: start, UsedPercent: 0}
	} else {
		known = false
		// Samples from the current window share (roughly) its reset time.
		for i := len(hist) - 1; i >= 0; i-- {
			h := hist[i]
			if last.ResetsAt != 0 && math.Abs(float64(h.ResetsAt-last.ResetsAt)) > 120 {
				break
			}
			if h.UsedPercent > last.UsedPercent+0.5 { // usage dropped since: an earlier window
				break
			}
			first = h
		}
	}
	elapsed := float64(last.TS - first.TS)
	if elapsed < 10*60 { // too early in the window to call a pace
		return "", ""
	}
	rate := (last.UsedPercent - first.UsedPercent) / elapsed // percent per second
	now := float64(time.Now().Unix())
	if rate <= 0 {
		// Flat readings only mean something if they cover the whole window so far.
		if resetsAt > 0 && known {
			return fmt.Sprintf("At this pace: stays near %.0f%% until the reset", last.UsedPercent), "ok"
		}
		return "", ""
	}
	toFull := (100 - last.UsedPercent) / rate
	if resetsAt == 0 {
		return "At this pace: hits 100% in about " + human(time.Duration(toFull)*time.Second), "warn"
	}
	left := float64(resetsAt) - now
	if left <= 0 {
		return "", ""
	}
	if toFull < left {
		return fmt.Sprintf("At this pace: hits 100%% in about %s, %s before it resets",
			human(time.Duration(toFull)*time.Second), human(time.Duration(left-toFull)*time.Second)), "crit"
	}
	at := last.UsedPercent + rate*left
	return fmt.Sprintf("At this pace: about %.0f%% by the reset", at), level(at)
}

type spendForecast struct {
	Basis       int
	Rate        float64 // USD per day
	SpentMonth  float64
	MonthTotal  float64
	MonthName   string
	HasBalance  bool
	BalanceDays float64
	BalanceDate string
	BalanceLvl  string
	HasLimit    bool
	LimitDays   float64
	LimitDate   string
	LimitHits   bool   // reached before the limit resets
	LimitReset  string // reset date, if known
	LimitLeft   float64
	Summary     string // one line for the dashboard card
}

var dollarsOf = regexp.MustCompile(`^\$([0-9.]+) of \$([0-9.]+)`)

// spendForecast is a straight-line projection from the average daily spend
// over the last `basis` days. days must cover at least 30 days, oldest first.
func (s *Server) spendForecast(days []store.DayTotal, basis int, balance *float64, quotas []quotaView) *spendForecast {
	basis = min(basis, len(days))
	f := &spendForecast{Basis: basis}
	for _, d := range days[len(days)-basis:] {
		f.Rate += d.USD
	}
	f.Rate /= float64(basis)

	now := time.Now().UTC()
	monthStart := now.Format("2006-01-")
	for _, d := range days {
		if strings.HasPrefix(d.Day, monthStart) {
			f.SpentMonth += d.USD
		}
	}
	daysInMonth := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	f.MonthTotal = f.SpentMonth + f.Rate*float64(daysInMonth-now.Day())
	f.MonthName = now.Format("January")

	var parts []string
	if balance != nil && f.Rate > 0 {
		f.HasBalance = true
		f.BalanceDays = *balance / f.Rate
		f.BalanceDate = shortDate(now.Add(time.Duration(f.BalanceDays * 24 * float64(time.Hour))))
		f.BalanceLvl = "text"
		switch {
		case f.BalanceDays < 14:
			f.BalanceLvl = "crit"
		case f.BalanceDays < 30:
			f.BalanceLvl = "warn"
		}
		parts = append(parts, "balance lasts to ~"+f.BalanceDate)
	}
	for _, q := range quotas {
		m := dollarsOf.FindStringSubmatch(q.Detail)
		if m == nil || f.Rate <= 0 {
			continue
		}
		used, _ := strconv.ParseFloat(m[1], 64)
		limit, _ := strconv.ParseFloat(m[2], 64)
		f.HasLimit = true
		f.LimitLeft = math.Max(0, limit-used)
		f.LimitDays = f.LimitLeft / f.Rate
		hit := now.Add(time.Duration(f.LimitDays * 24 * float64(time.Hour)))
		f.LimitDate = shortDate(hit)
		f.LimitHits = q.ResetsAt == 0 || hit.Unix() < q.ResetsAt
		if q.ResetsAt > 0 {
			f.LimitReset = shortDate(time.Unix(q.ResetsAt, 0))
		}
		if f.LimitHits {
			parts = append([]string{"limit hit ~" + f.LimitDate}, parts...)
		}
		break
	}
	if len(parts) > 0 {
		f.Summary = "At this pace: " + strings.Join(parts, " · ")
	}
	return f
}

func shortDate(t time.Time) string {
	if t.Year() != time.Now().Year() {
		return t.Format("Jan 2, 2006")
	}
	return t.Format("Jan 2")
}

// ---- charts ----

// dayBars renders daily bars as heights relative to the busiest day.
func dayBars(days []store.DayTotal, money bool, _ string) []bar {
	val := func(d store.DayTotal) float64 {
		if money {
			return d.USD
		}
		return float64(d.Tokens())
	}
	top := 0.0
	for _, d := range days {
		top = math.Max(top, val(d))
	}
	out := make([]bar, len(days))
	for i, d := range days {
		h := 0.0
		if top > 0 {
			h = val(d) / top * 100
		}
		label := tokens(int64(val(d))) + " tokens"
		if money {
			label = fmt.Sprintf("$%.2f", val(d))
		}
		out[i] = bar{Style: template.CSS(fmt.Sprintf("height:%.1f%%", h)), Title: dayLabel(d.Day) + ": " + label}
		if i == len(days)-1 {
			out[i].Class = "today"
		}
	}
	return out
}

func dayLabel(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	if day == time.Now().UTC().Format(time.DateOnly) {
		return "Today"
	}
	return shortDate(t)
}

// detailChart is the account page's 30-day chart plus a 14-day forecast.
type detailChart struct {
	Metric   string // "spend" or "tokens"
	Bars     []bar
	Axis     []string // top to bottom
	Start    string
	FcEnd    string
	HasFc    bool
	OverFrom string // forecast date the limit is passed, if any
}

func buildDetailChart(days []store.DayTotal, metric string, f *spendForecast) detailChart {
	c := detailChart{Metric: metric, Start: dayLabel(days[0].Day)}
	money := metric == "spend"
	val := func(d store.DayTotal) float64 {
		if money {
			return d.USD
		}
		return float64(d.Tokens())
	}
	// Tokens per dollar over the basis period scale the forecast in token mode.
	fc := 0.0
	if f != nil && f.Rate > 0 {
		c.HasFc = true
		fc = f.Rate
		if !money {
			var usd, tok float64
			for _, d := range days[len(days)-f.Basis:] {
				usd += d.USD
				tok += float64(d.Tokens())
			}
			if usd > 0 {
				fc = f.Rate * tok / usd
			} else {
				c.HasFc = false
			}
		}
	}
	top := fc
	for _, d := range days {
		top = math.Max(top, val(d))
	}
	top = niceCeil(top)
	format := func(x float64) string {
		if money {
			if top < 10 {
				return fmt.Sprintf("$%.2f", x)
			}
			return fmt.Sprintf("$%.0f", x)
		}
		return tokens(int64(x))
	}
	c.Axis = []string{format(top), format(top / 2), format(0)}
	h := func(x float64) float64 {
		if top == 0 {
			return 0
		}
		return x / top * 100
	}
	for i, d := range days {
		b := bar{Style: template.CSS(fmt.Sprintf("height:%.1f%%", h(val(d)))), Title: dayLabel(d.Day) + ": " + format(val(d))}
		if i == len(days)-1 {
			b.Class = "today"
		}
		c.Bars = append(c.Bars, b)
	}
	if c.HasFc {
		now := time.Now().UTC()
		for j := 1; j <= 14; j++ {
			b := bar{Style: template.CSS(fmt.Sprintf("height:%.1f%%", h(fc))), Class: "fc",
				Title: shortDate(now.AddDate(0, 0, j)) + " (forecast): " + format(fc)}
			if f.HasLimit && f.LimitHits && float64(j) > f.LimitDays {
				b.Class = "fc over"
				b.Title += " · past the limit, requests would fail"
				if c.OverFrom == "" {
					c.OverFrom = f.LimitDate
				}
			}
			c.Bars = append(c.Bars, b)
		}
		c.FcEnd = shortDate(now.AddDate(0, 0, 14))
	}
	return c
}

// niceCeil rounds up to 1, 2 or 5 times a power of ten.
func niceCeil(x float64) float64 {
	if x <= 0 {
		return 0
	}
	p := math.Pow(10, math.Floor(math.Log10(x)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*p >= x {
			return m * p
		}
	}
	return 10 * p
}

// downsample keeps at most n points, taking the max of each bucket so spikes survive.
func downsample(vals []float64, n int) []float64 {
	if len(vals) <= n {
		return vals
	}
	out := make([]float64, n)
	for i := range out {
		lo, hi := i*len(vals)/n, (i+1)*len(vals)/n
		m := vals[lo]
		for _, x := range vals[lo:hi] {
			m = math.Max(m, x)
		}
		out[i] = m
	}
	return out
}

func sparkline(vals []float64, top float64) template.HTML {
	if len(vals) < 2 {
		return ""
	}
	const w, h = 160.0, 32.0
	var pts strings.Builder
	for i, x := range vals {
		fmt.Fprintf(&pts, "%.1f,%.1f ", float64(i)*w/float64(len(vals)-1), h-2-math.Min(x, top)/top*(h-4))
	}
	return template.HTML(fmt.Sprintf(`<svg class="spark" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" aria-hidden="true"><polyline points="%s"/></svg>`,
		w, h, strings.TrimSpace(pts.String())))
}

// windowChart draws a limit window's history with the warning threshold.
func (s *Server) windowChart(hist []store.QuotaSample, label string) template.HTML {
	vals := downsample(percents(hist), 300)
	if len(vals) < 2 {
		return template.HTML(`<p class="muted small chart-empty">Not enough history yet.</p>`)
	}
	const w, h = 600.0, 70.0
	y := func(p float64) float64 { return h - 1 - clamp(p, 0, 100)/100*(h-2) }
	var line strings.Builder
	for i, x := range vals {
		fmt.Fprintf(&line, "%.1f,%.1f ", float64(i)*w/float64(len(vals)-1), y(x))
	}
	pts := strings.TrimSpace(line.String())
	warnY := y(float64(s.settings().Warn))
	return template.HTML(fmt.Sprintf(`<svg class="win-chart" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="%s use history">`+
		`<polygon class="area" points="0,%.0f %s %.0f,%.0f"/><polyline class="line" points="%s"/>`+
		`<line class="warn-line" x1="0" x2="%.0f" y1="%.1f" y2="%.1f"/></svg>`,
		w, h, template.HTMLEscapeString(label), h, pts, w, h, pts, w, warnY, warnY))
}

// ---- formatting ----

func until(unix int64) string {
	if unix == 0 {
		return ""
	}
	d := time.Until(time.Unix(unix, 0))
	if d <= 0 {
		return "now"
	}
	if d > 48*time.Hour {
		return "on " + shortDate(time.Unix(unix, 0))
	}
	return "in " + human(d)
}

func ago(unix int64) string {
	if unix == 0 {
		return "never"
	}
	d := time.Since(time.Unix(unix, 0))
	if d < time.Minute {
		return "just now"
	}
	return human(d) + " ago"
}

func human(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dm", max(1, int(math.Ceil(d.Minutes()))))
	}
}

func tokens(n int64) string {
	f := float64(n)
	switch {
	case f >= 1e9:
		return fmt.Sprintf("%.2fB", f/1e9)
	case f >= 1e6:
		return fmt.Sprintf("%.1fM", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.1fk", f/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func money(f float64) string {
	if f >= 1000 {
		return "$" + strconv.FormatFloat(math.Round(f), 'f', 0, 64)
	}
	return fmt.Sprintf("$%.2f", f)
}

// browserName gives a short "Firefox on Linux" style label for a session.
func browserName(ua string) string {
	browser, osName := "Browser", ""
	switch {
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	case strings.Contains(ua, "curl/"):
		browser = "curl"
	}
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		osName = "iOS"
	case strings.Contains(ua, "Android"):
		osName = "Android"
	case strings.Contains(ua, "Mac OS X"):
		osName = "macOS"
	case strings.Contains(ua, "Windows"):
		osName = "Windows"
	case strings.Contains(ua, "Linux"):
		osName = "Linux"
	}
	if osName == "" {
		return browser
	}
	return browser + " on " + osName
}

var funcs = template.FuncMap{
	"money":   money,
	"tokens":  tokens,
	"ago":     ago,
	"pct":     func(f float64) string { return fmt.Sprintf("%.0f%%", f) },
	"deref":   func(f *float64) float64 { return *f },
	"date":    func(unix int64) string { return shortDate(time.Unix(unix, 0)) },
	"browser": browserName,
	"initial": func(s string) string {
		if s == "" {
			return "?"
		}
		return strings.ToUpper(s[:1])
	},
	"days":  func(d float64) string { return strconv.Itoa(int(math.Round(d))) },
	"sub":   func(a, b float64) float64 { return a - b },
	"float": func(n int) float64 { return float64(n) },
	"div":   func(a, b int) int { return a / b },
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
}
