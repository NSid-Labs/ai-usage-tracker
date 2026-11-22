package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

func TestSaveAndAggregate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.CreateAccount("a", "anthropic-api", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format(time.DateOnly)
	res := &core.Result{
		Usage: []core.DailyUsage{
			{Day: today, Model: "m1", Input: 10, Output: 5},
			{Day: today, Model: "m2", Input: 1, CacheRead: 100},
		},
		Costs:  []core.DailyCost{{Day: today, USD: 1.5}},
		Quotas: []core.Quota{{Window: "5h", UsedPercent: 42}},
		Plan:   "max",
	}
	// Saving twice must upsert, not double count.
	for range 2 {
		if err := st.SaveResult(id, res); err != nil {
			t.Fatal(err)
		}
	}
	days, err := st.DailyTotals(id, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 30 {
		t.Fatalf("want 30 days, got %d", len(days))
	}
	last := days[29]
	if last.Day != today || last.USD != 1.5 || last.Tokens() != 116 {
		t.Errorf("unexpected today total: %+v", last)
	}
	q, err := st.LatestQuotas(id)
	if err != nil || len(q) != 1 || q[0].UsedPercent != 42 {
		t.Errorf("quotas = %+v, %v", q, err)
	}
	a, _ := st.GetAccount(id)
	if a.Plan != "max" || a.LastPoll == 0 {
		t.Errorf("account not updated: %+v", a)
	}
	rows, err := st.DailyUsageSince(today, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AccountID != id || rows[0].USD != 1.5 || rows[0].Tokens() != 116 {
		t.Errorf("history rows = %+v", rows)
	}
	models, err := st.ModelTotalsSince(today, 0)
	if err != nil || len(models) != 2 || models[0].Model != "m2" {
		t.Errorf("history models = %+v, %v", models, err)
	}
	if err := st.DeleteAccount(id); err != nil {
		t.Fatal(err)
	}
	if days, _ := st.DailyTotals(id, 1); days[0].Tokens() != 0 {
		t.Error("cascade delete failed")
	}
}
