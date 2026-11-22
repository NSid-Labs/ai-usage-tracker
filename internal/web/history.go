package web

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/providers"
	"github.com/Dukko/ai-usage-tracker/internal/store"
)

// filterOption is a plain <option> for the account picker on the History page.
type filterOption struct {
	ID       string
	Label    string
	Selected bool
}

type historyBucket struct {
	Key      string // day ("2006-01-02") or month ("2006-01")
	Label    string // short label for the axis and table
	Title    string // full tooltip with spend, tokens and requests
	USD      float64
	Tokens   int64
	Requests int64
	Bar      bar
}

type historyAccountRow struct {
	ID            int64
	Name          string
	ProviderLabel string
	Badge         template.CSS
	Initials      string
	USD           float64
	Tokens        int64
	Requests      int64
	Share         template.CSS
	SharePct      float64
}

type historyLimit struct {
	AccountID   int64
	AccountName string
	Label       string
	UsedPercent float64
	ResetsIn    string
	Detail      string
	Level       string
	Peak        float64
	Fill        template.CSS
	Chart       template.HTML
}

type historyChart struct {
	Metric string // "spend" or "tokens"
	Per    string // "daily" or "monthly"
	Bars   []bar
	Axis   []string // top to bottom
	Start  string
	End    string
}

type historyPage struct {
	RangeDays    int
	RangeLabel   string
	Ranges       []option
	Accounts     []filterOption
	AccountID    int64
	AccountName  string
	Metric       string
	Metrics      []option
	TotalSpend   float64
	TotalTokens  int64
	TotalReqs    int64
	PrevSpend    float64
	PrevTokens   int64
	SpendDelta   string
	TokenDelta   string
	AvgValue     string
	AvgSub       string
	HasCost      bool
	HasData      bool
	Subscription bool
	Chart        historyChart
	Buckets      []historyBucket // newest first, for the table
	Rows         []historyAccountRow
	Models       []modelRow
	Limits       []historyLimit
}

func historyDays(v string) int {
	switch v {
	case "7":
		return 7
	case "90":
		return 90
	case "365":
		return 365
	default:
		return 30
	}
}

func historyRangeLabel(days int) string {
	if days == 365 {
		return "12 months"
	}
	return fmt.Sprintf("%d days", days)
}

func historyDelta(cur, prev float64) string {
	if prev <= 0 {
		if cur > 0 {
			return "no earlier period to compare"
		}
		return ""
	}
	change := (cur - prev) / prev * 100
	if math.Abs(change) < 0.5 {
		return "about the same as the previous period"
	}
	word := "higher"
	if change < 0 {
		word = "lower"
	}
	return fmt.Sprintf("%.0f%% %s than the previous period", math.Abs(change), word)
}

// history shows past consumption across every account: daily or monthly
// totals, a per-account split, per-model tokens and the subscription limit
// history recorded so far. Unlike the dashboard, it does not forecast.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.st.ListAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	days := historyDays(r.URL.Query().Get("range"))
	startT := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	start := startT.Format(time.DateOnly)
	prevStart := startT.AddDate(0, 0, -days).Format(time.DateOnly)

	var selected store.Account
	var accountID int64
	if v := r.URL.Query().Get("account"); v != "" && v != "0" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			for _, a := range accounts {
				if a.ID == id {
					selected, accountID = a, id
					break
				}
			}
		}
	}

	p := historyPage{RangeDays: days, RangeLabel: historyRangeLabel(days), AccountID: accountID}
	if accountID > 0 {
		p.AccountName = selected.Name
		if prov, ok := providers.Get(selected.Provider); ok && prov.Kind() == providers.KindOAuth {
			p.Subscription = true
		}
	}

	rows, err := s.st.DailyUsageSince(prevStart, accountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var current []store.AccountDayTotal
	for _, row := range rows {
		if row.Day >= start {
			current = append(current, row)
			p.TotalSpend += row.USD
			p.TotalTokens += row.Tokens()
			p.TotalReqs += row.Requests
		} else {
			p.PrevSpend += row.USD
			p.PrevTokens += row.Tokens()
		}
	}
	p.HasCost = p.TotalSpend > 0
	p.HasData = p.TotalSpend > 0 || p.TotalTokens > 0 || p.TotalReqs > 0

	metric := r.URL.Query().Get("metric")
	if metric != "spend" && metric != "tokens" {
		metric = "tokens"
		if p.HasCost {
			metric = "spend"
		}
	}
	if metric == "spend" && !p.HasCost {
		metric = "tokens"
	}
	p.Metric = metric

	// Range and metric links keep the account filter.
	link := func(rangeDays int, metric string) string {
		q := url.Values{}
		q.Set("range", strconv.Itoa(rangeDays))
		if accountID > 0 {
			q.Set("account", strconv.FormatInt(accountID, 10))
		}
		if metric != "" && !p.Subscription {
			q.Set("metric", metric)
		}
		return "/history?" + q.Encode()
	}
	for _, d := range []int{7, 30, 90, 365} {
		p.Ranges = append(p.Ranges, option{historyRangeLabel(d), link(d, metric), d == days})
	}
	if p.HasCost {
		p.Metrics = []option{{"Spend", link(days, "spend"), metric == "spend"}, {"Tokens", link(days, "tokens"), metric == "tokens"}}
	}
	p.Accounts = []filterOption{{ID: "0", Label: "All accounts", Selected: accountID == 0}}
	for _, a := range accounts {
		p.Accounts = append(p.Accounts, filterOption{ID: strconv.FormatInt(a.ID, 10), Label: a.Name, Selected: a.ID == accountID})
	}

	// Buckets: days for normal ranges, months for a full year.
	monthly := days > 90
	p.Chart.Per = "Daily"
	if monthly {
		p.Chart.Per = "Monthly"
	}
	buckets := map[string]*historyBucket{}
	var keys []string
	addBucket := func(key, label string) {
		if _, ok := buckets[key]; ok {
			return
		}
		buckets[key] = &historyBucket{Key: key, Label: label}
		keys = append(keys, key)
	}
	if monthly {
		firstMonth := time.Date(startT.Year(), startT.Month(), 1, 0, 0, 0, 0, time.UTC)
		now := time.Now().UTC()
		lastMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		for m := firstMonth; !m.After(lastMonth); m = m.AddDate(0, 1, 0) {
			addBucket(m.Format("2006-01"), m.Format("Jan 2006"))
		}
	} else {
		for i := 0; i < days; i++ {
			d := startT.AddDate(0, 0, i)
			addBucket(d.Format(time.DateOnly), dayLabel(d.Format(time.DateOnly)))
		}
	}
	for _, row := range current {
		key := row.Day
		if monthly {
			if len(row.Day) < 7 {
				continue
			}
			key = row.Day[:7]
		}
		b := buckets[key]
		if b == nil {
			continue
		}
		b.USD += row.USD
		b.Tokens += row.Tokens()
		b.Requests += row.Requests
	}

	metricValue := func(b historyBucket) float64 {
		if metric == "spend" {
			return b.USD
		}
		return float64(b.Tokens)
	}
	top := 0.0
	for _, k := range keys {
		top = math.Max(top, metricValue(*buckets[k]))
	}
	chartTop := niceCeil(top)
	height := func(v float64) float64 {
		if chartTop <= 0 {
			return 0
		}
		return v / chartTop * 100
	}
	for i, k := range keys {
		b := buckets[k]
		class := ""
		if i == len(keys)-1 {
			class = "today"
		}
		b.Bar = bar{
			Style: template.CSS(fmt.Sprintf("height:%.1f%%", height(metricValue(*b)))),
			Title: fmt.Sprintf("%s: %s · %s tokens · %s requests", b.Label, money(b.USD), tokens(b.Tokens), tokens(b.Requests)),
			Class: class,
		}
		p.Chart.Bars = append(p.Chart.Bars, b.Bar)
		p.Buckets = append([]historyBucket{*b}, p.Buckets...) // newest first
	}
	if len(keys) > 0 {
		p.Chart.Start = buckets[keys[0]].Label
		p.Chart.End = buckets[keys[len(keys)-1]].Label
	}
	format := func(v float64) string {
		if metric == "spend" {
			return money(v)
		}
		return tokens(int64(v))
	}
	p.Chart.Axis = []string{format(chartTop), format(chartTop / 2), format(0)}
	p.Chart.Metric = metric

	p.SpendDelta = historyDelta(p.TotalSpend, p.PrevSpend)
	p.TokenDelta = historyDelta(float64(p.TotalTokens), float64(p.PrevTokens))
	if metric == "spend" {
		p.AvgValue = money(p.TotalSpend / float64(days))
		p.AvgSub = "spend per day"
	} else {
		p.AvgValue = tokens(int64(float64(p.TotalTokens) / float64(days)))
		p.AvgSub = "tokens per day"
	}

	// Per-account split for the current range only.
	byID := map[int64]*historyAccountRow{}
	for _, a := range accounts {
		byID[a.ID] = &historyAccountRow{ID: a.ID, Name: a.Name, ProviderLabel: a.Provider}
	}
	for _, row := range current {
		acc := byID[row.AccountID]
		if acc == nil {
			continue
		}
		acc.USD += row.USD
		acc.Tokens += row.Tokens()
		acc.Requests += row.Requests
	}
	for _, a := range accounts {
		row := byID[a.ID]
		if row == nil || (row.USD == 0 && row.Tokens == 0 && row.Requests == 0) {
			continue
		}
		if metric == "spend" {
			if p.TotalSpend > 0 {
				row.SharePct = row.USD / p.TotalSpend * 100
			}
		} else if p.TotalTokens > 0 {
			row.SharePct = float64(row.Tokens) / float64(p.TotalTokens) * 100
		}
		row.Share = template.CSS(fmt.Sprintf("width:%.1f%%", row.SharePct))
		color, initials := providerStyle(a.Provider)
		row.Badge, row.Initials = template.CSS("background:"+color), initials
		if prov, ok := providers.Get(a.Provider); ok {
			row.ProviderLabel = prov.Label()
		}
		p.Rows = append(p.Rows, *row)
	}
	sort.SliceStable(p.Rows, func(i, j int) bool {
		if metric == "spend" {
			return p.Rows[i].USD > p.Rows[j].USD
		}
		return p.Rows[i].Tokens > p.Rows[j].Tokens
	})

	models, err := s.st.ModelTotalsSince(start, accountID)
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

	// Subscription limit history sits next to spend history: same page, same range.
	for _, a := range accounts {
		if accountID > 0 && a.ID != accountID {
			continue
		}
		prov, ok := providers.Get(a.Provider)
		if !ok || prov.Kind() != providers.KindOAuth {
			continue
		}
		quotas, err := s.st.LatestQuotas(a.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, q := range quotas {
			hist, err := s.st.QuotaHistory(a.ID, q.Window, startT)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if len(hist) == 0 {
				continue
			}
			peak := 0.0
			for _, h := range hist {
				peak = math.Max(peak, h.UsedPercent)
			}
			label := windowLabel(q.Window)
			p.Limits = append(p.Limits, historyLimit{
				AccountID: a.ID, AccountName: a.Name, Label: label,
				UsedPercent: q.UsedPercent, ResetsIn: until(q.ResetsAt), Detail: q.Detail,
				Level: s.level(q.UsedPercent), Peak: peak,
				Fill:  template.CSS(fmt.Sprintf("width:%.1f%%", clamp(q.UsedPercent, 0, 100))),
				Chart: s.windowChart(hist, label),
			})
		}
	}

	s.render(w, r, "history", "History", "history", p)
}
