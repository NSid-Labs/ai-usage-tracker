package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

// refuseless tracks the prepaid credit of a Refuseless API key. It is an
// OpenAI-compatible endpoint that bills per token out of a credit balance, so
// there is no reset window: what matters is what is left.
//
// GET /v1/credits answers:
//
//	{"balance":2.89,"reserved":0,"available":2.89,"key_limit":null,
//	 "key_spent":2.10,"unit":"usd","updated_at":...}
//
// A key can also carry its own spending limit, in which case that is shown as a
// quota bar as well.
type refuseless struct{}

func init() { register(refuseless{}) }

var refuselessBase = strings.TrimRight(envOr("REFUSELESS_BASE_URL", "https://api.refuseless.com"), "/")

func (refuseless) ID() string    { return "refuseless" }
func (refuseless) Label() string { return "Refuseless credits" }
func (refuseless) Kind() string  { return KindAPIKey }

func (refuseless) KeyHint() string { return "API key; shows the credit balance" }

type refuselessCredits struct {
	Balance   float64  `json:"balance"`
	Available float64  `json:"available"`
	Reserved  float64  `json:"reserved"`
	KeyLimit  *float64 `json:"key_limit"`
	KeySpent  float64  `json:"key_spent"`
	Unit      string   `json:"unit"`
}

func (r refuseless) Fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	if c.APIKey == "" {
		return nil, errors.New("no Refuseless API key stored")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, refuselessBase+"/v1/credits", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	var out refuselessCredits
	if err := doJSON(req, &out); err != nil {
		return nil, err
	}

	available := out.Available
	if available == 0 && out.Balance != 0 {
		available = out.Balance
	}
	res := &core.Result{Balance: &available}

	if out.KeyLimit != nil && *out.KeyLimit > 0 {
		res.Quotas = append(res.Quotas, core.Quota{
			Window:      "key limit",
			UsedPercent: out.KeySpent / *out.KeyLimit * 100,
			Detail:      fmt.Sprintf("$%.2f of $%.2f", out.KeySpent, *out.KeyLimit),
		})
	}
	return res, nil
}
