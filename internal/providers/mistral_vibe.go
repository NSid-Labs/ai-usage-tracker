package providers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

// mistralVibe tracks a Mistral Vibe Code subscription's monthly allowance.
//
// It reads the public billing API the Vibe CLI itself uses for its footer gauge
// (vibe/app_server/_billing.py in mistralai/mistral-vibe):
//
//	GET /v1/billing/subscription  the organization's plan and whether pay-as-you-go is on
//	GET /v1/billing/budget        how much of the monthly Vibe Code allowance is used
//
// Both endpoints are optional upstream. /budget needs the
// see_org_inference_usage permission; a platform that doesn't serve billing
// answers 404. Neither is a failure of the account itself, so the plan is still
// reported when only the allowance is missing — but an account with nothing to
// plot returns an error, so the dashboard says why instead of showing an empty
// card.
type mistralVibe struct{}

func init() { register(mistralVibe{}) }

var mistralBillingBase = strings.TrimRight(envOr("MISTRAL_BILLING_BASE", "https://api.mistral.ai/v1"), "/")

func (mistralVibe) ID() string    { return "mistral-vibe" }
func (mistralVibe) Label() string { return "Mistral Vibe Code" }
func (mistralVibe) Kind() string  { return KindAPIKey }

func (mistralVibe) KeyHint() string {
	return "Vibe API key; the monthly allowance needs the usage permission"
}

type mistralSubscription struct {
	PlanName        string `json:"plan_name"`
	VibePaygEnabled *bool  `json:"vibe_payg_enabled"`
}

type mistralBudgetDetail struct {
	UsagePercentage float64 `json:"usage_percentage"`
	InitialBudget   float64 `json:"initial_budget"`
	ResetAt         string  `json:"reset_at"`
	PaygEnabled     *bool   `json:"payg_enabled"`
}

type mistralBudget struct {
	VibeBudget *mistralBudgetDetail `json:"vibe_budget"`
}

func mistralGet(ctx context.Context, apiKey, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mistralBillingBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return doJSON(req, out)
}

// mistralPlanName turns the billing plan's code into the label Mistral's own
// console shows. Unknown names are passed through unchanged.
func mistralPlanName(name string) string {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "":
		return ""
	case "FREE":
		return "Free"
	case "INDIVIDUAL":
		return "Pro"
	case "EDU":
		return "Student"
	case "TEAM":
		return "Team"
	case "F":
		return "Mistral Code Free"
	case "E":
		return "Mistral Code Enterprise"
	default:
		return strings.TrimSpace(name)
	}
}

func (mistralVibe) Fetch(ctx context.Context, c *core.Credentials) (*core.Result, error) {
	if c.APIKey == "" {
		return nil, errors.New("no Mistral API key stored")
	}
	res := &core.Result{}

	var sub mistralSubscription
	subErr := mistralGet(ctx, c.APIKey, "/billing/subscription", &sub)
	if isUnauthorized(subErr) {
		return nil, errors.New("Mistral rejected the API key")
	}

	var budget mistralBudget
	budgetErr := mistralGet(ctx, c.APIKey, "/billing/budget", &budget)
	if isUnauthorized(budgetErr) {
		return nil, errors.New("the key cannot read Vibe usage: it needs the see_org_inference_usage permission")
	}

	res.Plan = mistralPlanName(sub.PlanName)
	if res.Plan == "" {
		res.Plan = c.Plan
	}

	if b := budget.VibeBudget; b != nil && b.InitialBudget > 0 {
		payg := false
		if b.PaygEnabled != nil {
			payg = *b.PaygEnabled
		} else if sub.VibePaygEnabled != nil {
			payg = *sub.VibePaygEnabled
		}
		q := core.Quota{
			Window:      "monthly",
			UsedPercent: math.Max(0, math.Min(100, b.UsagePercentage)),
			// The API reports no currency; print the amounts as they come.
			Detail: fmt.Sprintf("%.2f of %.2f", b.InitialBudget*b.UsagePercentage/100, b.InitialBudget),
		}
		if payg {
			q.Detail += " · PAYG"
		}
		if t, err := time.Parse(time.RFC3339, b.ResetAt); err == nil {
			q.ResetsAt = t.Unix()
		}
		res.Quotas = append(res.Quotas, q)
	}

	if len(res.Quotas) == 0 {
		switch {
		case budgetErr != nil:
			return nil, fmt.Errorf("no monthly allowance from Mistral (billing/budget: %v)", budgetErr)
		case subErr != nil:
			return nil, fmt.Errorf("no account details from Mistral (billing/subscription: %v)", subErr)
		default:
			return nil, errors.New("this Mistral key has no Vibe Code allowance to track")
		}
	}
	return res, nil
}
