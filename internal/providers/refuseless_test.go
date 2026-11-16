package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

func startRefuseless(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := refuselessBase
	refuselessBase = srv.URL
	t.Cleanup(func() { refuselessBase = old })
}

func TestRefuselessBalance(t *testing.T) {
	startRefuseless(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/credits" || r.Header.Get("Authorization") != "Bearer key1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"object":"credits","balance":2.890971,"reserved":0,"available":2.890971,
			"key_limit":null,"key_spent":2.109029,"unit":"usd"}`))
	})

	res, err := refuseless{}.Fetch(context.Background(), &core.Credentials{APIKey: "key1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Balance == nil || *res.Balance < 2.88 || *res.Balance > 2.90 {
		t.Fatalf("balance = %v", res.Balance)
	}
	if len(res.Quotas) != 0 {
		t.Errorf("a key without a limit should have no quota: %+v", res.Quotas)
	}
}

func TestRefuselessKeyLimit(t *testing.T) {
	startRefuseless(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"balance":8,"available":8,"key_limit":10,"key_spent":2.5,"unit":"usd"}`))
	})
	res, err := refuseless{}.Fetch(context.Background(), &core.Credentials{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Quotas) != 1 || res.Quotas[0].UsedPercent != 25 || res.Quotas[0].Detail != "$2.50 of $10.00" {
		t.Fatalf("quotas = %+v", res.Quotas)
	}
}

func TestRefuselessNeedsKey(t *testing.T) {
	if _, err := (refuseless{}).Fetch(context.Background(), &core.Credentials{}); err == nil {
		t.Fatal("want an error without a key")
	}
}
