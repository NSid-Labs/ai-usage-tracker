package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

func startMistralBilling(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := mistralBillingBase
	mistralBillingBase = srv.URL + "/v1"
	t.Cleanup(func() { mistralBillingBase = old })
}

func TestMistralVibeFetch(t *testing.T) {
	startMistralBilling(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vibe-key" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Unauthorized"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/billing/subscription":
			w.Write([]byte(`{"plan_name":"EDU","vibe_payg_enabled":false}`))
		case "/v1/billing/budget":
			w.Write([]byte(`{"vibe_budget":{"usage_percentage":5.082352941176471,
				"initial_budget":127.5,"reset_at":"2026-12-01T00:00:00Z","payg_enabled":true}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := mistralVibe{}.Fetch(context.Background(), &core.Credentials{APIKey: "vibe-key"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan != "Student" {
		t.Errorf("plan = %q, want Student", res.Plan)
	}
	if len(res.Quotas) != 1 {
		t.Fatalf("quotas = %+v", res.Quotas)
	}
	q := res.Quotas[0]
	if q.Window != "monthly" || q.UsedPercent < 5.08 || q.UsedPercent > 5.09 {
		t.Errorf("quota = %+v", q)
	}
	if q.Detail != "6.48 of 127.50 · PAYG" {
		t.Errorf("detail = %q", q.Detail)
	}
	if q.ResetsAt == 0 {
		t.Error("reset time not parsed")
	}
}

func TestMistralVibeMissingBudgetPermission(t *testing.T) {
	startMistralBilling(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/billing/budget" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"missing scope"}`))
			return
		}
		w.Write([]byte(`{"plan_name":"INDIVIDUAL"}`))
	})

	_, err := mistralVibe{}.Fetch(context.Background(), &core.Credentials{APIKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "see_org_inference_usage") {
		t.Fatalf("want a permission error, got %v", err)
	}
}

func TestMistralVibeRejectedKey(t *testing.T) {
	startMistralBilling(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Unauthorized"}`))
	})
	_, err := mistralVibe{}.Fetch(context.Background(), &core.Credentials{APIKey: "bad"})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("want a rejected-key error, got %v", err)
	}
}

func TestMistralPlanNames(t *testing.T) {
	for in, want := range map[string]string{
		"FREE": "Free", "individual": "Pro", "EDU": "Student", "TEAM": "Team", "": "",
	} {
		if got := mistralPlanName(in); got != want {
			t.Errorf("mistralPlanName(%q) = %q, want %q", in, got, want)
		}
	}
}
