package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"

	"agentd/internal/model"
	"agentd/internal/tools"
)

type Snapshot struct {
	AsOf         time.Time         `json:"as_of"`
	Source       string            `json:"source"`
	BaseCurrency string            `json:"base_currency"`
	FX           map[string]string `json:"fx,omitempty"`
	Positions    []Position        `json:"positions"`
}

type Position struct {
	Account    string `json:"account"`
	Symbol     string `json:"symbol"`
	AssetClass string `json:"asset_class,omitempty"`
	Currency   string `json:"currency"`
	Quantity   string `json:"quantity"`
	Price      string `json:"price,omitempty"`
	CostBasis  string `json:"cost_basis,omitempty"`
}

type Holding struct {
	Symbol     string   `json:"symbol"`
	AssetClass string   `json:"asset_class,omitempty"`
	Currency   string   `json:"currency"`
	Quantity   string   `json:"quantity"`
	Accounts   []string `json:"accounts"`
	Value      string   `json:"value_in_base,omitempty"`
	Weight     string   `json:"weight_pct,omitempty"`
	CostBasis  string   `json:"cost_basis,omitempty"`
}

type View struct {
	AsOf         time.Time `json:"as_of"`
	Source       string    `json:"source"`
	BaseCurrency string    `json:"base_currency"`
	Complete     bool      `json:"complete"`
	Warnings     []string  `json:"warnings,omitempty"`
	TotalValue   string    `json:"total_value_in_base,omitempty"`
	Holdings     []Holding `json:"holdings"`
}

func decimal(s string) (*big.Rat, bool) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	return r, ok
}

func Summarize(s Snapshot, now time.Time, maxAge time.Duration) (View, error) {
	v := View{AsOf: s.AsOf, Source: s.Source, BaseCurrency: strings.ToUpper(s.BaseCurrency), Complete: true}
	if v.BaseCurrency == "" || s.AsOf.IsZero() {
		return v, fmt.Errorf("snapshot needs as_of and base_currency")
	}
	if now.Sub(s.AsOf) > maxAge {
		v.Complete = false
		v.Warnings = append(v.Warnings, fmt.Sprintf("snapshot is %s old; positions may have changed", now.Sub(s.AsOf).Round(time.Hour)))
	}
	type agg struct {
		h        Holding
		qty      *big.Rat
		value    *big.Rat
		cost     *big.Rat
		unpriced bool
		nocost   bool
	}
	bySymbol := map[string]*agg{}
	for i, p := range s.Positions {
		symbol, currency := strings.ToUpper(strings.TrimSpace(p.Symbol)), strings.ToUpper(strings.TrimSpace(p.Currency))
		if symbol == "" || currency == "" {
			return v, fmt.Errorf("position %d needs a symbol and currency", i+1)
		}
		qty, ok := decimal(p.Quantity)
		if !ok {
			return v, fmt.Errorf("position %d (%s): quantity %q is not a number", i+1, symbol, p.Quantity)
		}
		key := symbol + "|" + currency
		a := bySymbol[key]
		if a == nil {
			a = &agg{h: Holding{Symbol: symbol, AssetClass: p.AssetClass, Currency: currency}, qty: new(big.Rat), value: new(big.Rat), cost: new(big.Rat)}
			bySymbol[key] = a
		}
		a.qty.Add(a.qty, qty)
		a.h.Accounts = append(a.h.Accounts, p.Account)
		if p.CostBasis == "" {
			a.nocost = true
		} else if c, ok := decimal(p.CostBasis); ok {
			a.cost.Add(a.cost, c)
		} else {
			return v, fmt.Errorf("position %d (%s): cost_basis %q is not a number", i+1, symbol, p.CostBasis)
		}
		if p.Price == "" {
			a.unpriced = true
			continue
		}
		price, ok := decimal(p.Price)
		if !ok {
			return v, fmt.Errorf("position %d (%s): price %q is not a number", i+1, symbol, p.Price)
		}
		rate := big.NewRat(1, 1)
		if currency != v.BaseCurrency {
			fx, ok := s.FX[currency]
			if !ok {
				a.unpriced = true
				v.Warnings = append(v.Warnings, fmt.Sprintf("%s: no %s→%s rate, value left out", symbol, currency, v.BaseCurrency))
				continue
			}
			if rate, ok = decimal(fx); !ok {
				return v, fmt.Errorf("fx rate for %s is not a number", currency)
			}
		}
		a.value.Add(a.value, new(big.Rat).Mul(new(big.Rat).Mul(qty, price), rate))
	}
	total := new(big.Rat)
	for _, a := range bySymbol {
		if !a.unpriced {
			total.Add(total, a.value)
		}
	}
	keys := make([]string, 0, len(bySymbol))
	for k := range bySymbol {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := bySymbol[k]
		a.h.Quantity = trim(a.qty.FloatString(8))
		if !a.nocost {
			a.h.CostBasis = a.cost.FloatString(2)
		}
		if len(a.h.Accounts) > 1 {
			v.Warnings = append(v.Warnings, fmt.Sprintf("%s is held in %d accounts; quantities are combined", a.h.Symbol, len(a.h.Accounts)))
		}
		if a.unpriced {
			v.Complete = false
			v.Warnings = append(v.Warnings, fmt.Sprintf("%s has no usable price; it is left out of the total and weights", a.h.Symbol))
		} else {
			a.h.Value = a.value.FloatString(2)
			if total.Sign() > 0 {
				a.h.Weight = new(big.Rat).Mul(new(big.Rat).Quo(a.value, total), big.NewRat(100, 1)).FloatString(2)
			}
		}
		v.Holdings = append(v.Holdings, a.h)
	}
	if total.Sign() > 0 {
		v.TotalValue = total.FloatString(2)
	}
	sort.Strings(v.Warnings)
	return v, nil
}

func trim(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

type Tool struct {
	Path   string
	Now    func() time.Time
	MaxAge time.Duration
}

func (*Tool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name: "portfolio_positions",
		Description: "Read the owner's current holdings: quantities, values in the base currency, weights, and warnings about stale or missing data. " +
			"Read-only. Never estimate values that are reported as missing.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (*Tool) Effect() tools.Effect { return tools.EffectRead }

func (*Tool) Resource(json.RawMessage) (string, error) { return "portfolio", nil }

func (t *Tool) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	raw, err := os.ReadFile(t.Path)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%w: holdings snapshot unavailable", tools.ErrNotExecuted)
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return tools.Result{}, fmt.Errorf("%w: holdings snapshot is not valid JSON", tools.ErrNotExecuted)
	}
	v, err := Summarize(s, t.Now(), t.MaxAge)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%w: holdings snapshot: %v", tools.ErrNotExecuted, err)
	}
	out, _ := json.Marshal(v)
	return tools.Result{Output: out}, nil
}

func (*Tool) Reconcile(context.Context, tools.Invocation) (tools.Result, tools.Finding, error) {
	return tools.Result{}, tools.NotHappened, nil
}
