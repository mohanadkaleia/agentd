package portfolio_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentd/internal/integrations/portfolio"
	"agentd/internal/tools"
)

var now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func snapshot(age time.Duration) portfolio.Snapshot {
	return portfolio.Snapshot{AsOf: now.Add(-age), Source: "manual", BaseCurrency: "usd", FX: map[string]string{"EUR": "1.10"},
		Positions: []portfolio.Position{
			{Account: "ira", Symbol: "aapl", Currency: "USD", Quantity: "10.5", Price: "200", CostBasis: "1500"},
			{Account: "taxable", Symbol: "AAPL", Currency: "USD", Quantity: "0.25", Price: "200", CostBasis: "40"},
			{Account: "taxable", Symbol: "SAP", Currency: "EUR", Quantity: "10", Price: "100"},
			{Account: "taxable", Symbol: "TINY", Currency: "USD", Quantity: "3"},
			{Account: "taxable", Symbol: "NESN", Currency: "CHF", Quantity: "1", Price: "90"},
		}}
}

func TestWeightsUseExactDecimalsAndFX(t *testing.T) {
	v, err := portfolio.Summarize(snapshot(time.Hour), now, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]portfolio.Holding{}
	for _, h := range v.Holdings {
		got[h.Symbol] = h
	}
	if a := got["AAPL"]; a.Quantity != "10.75" || a.Value != "2150.00" || a.Weight != "66.15" || a.CostBasis != "1540.00" || len(a.Accounts) != 2 {
		t.Fatalf("AAPL = %+v", a)
	}
	if s := got["SAP"]; s.Value != "1100.00" || s.Weight != "33.85" || s.CostBasis != "" {
		t.Fatalf("SAP = %+v (cost basis must be left out, not zero)", s)
	}
	if v.TotalValue != "3250.00" || v.Complete {
		t.Fatalf("total = %s complete = %v", v.TotalValue, v.Complete)
	}
	for _, h := range []string{"TINY", "NESN"} {
		if got[h].Value != "" || got[h].Weight != "" {
			t.Fatalf("%s = %+v, want no invented value", h, got[h])
		}
	}
	joined := strings.Join(v.Warnings, "\n")
	for _, want := range []string{"TINY has no usable price", "no CHF→USD rate", "AAPL is held in 2 accounts"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q:\n%s", want, joined)
		}
	}
}

func TestStaleOrBrokenSnapshotsSaySo(t *testing.T) {
	v, err := portfolio.Summarize(snapshot(5*24*time.Hour), now, 72*time.Hour)
	if err != nil || v.Complete || !strings.Contains(strings.Join(v.Warnings, " "), "old") {
		t.Fatalf("stale snapshot = %+v, %v", v, err)
	}
	bad := snapshot(time.Hour)
	bad.Positions[0].Quantity = "ten"
	if _, err := portfolio.Summarize(bad, now, 72*time.Hour); err == nil {
		t.Fatal("accepted a non-numeric quantity")
	}
}

func TestToolIsReadOnlyAndFailsHonestly(t *testing.T) {
	tool := &portfolio.Tool{Path: filepath.Join(t.TempDir(), "missing.json"), Now: func() time.Time { return now }, MaxAge: 72 * time.Hour}
	if tool.Effect() != tools.EffectRead {
		t.Fatal("holdings tool must be read-only")
	}
	if _, err := tool.Execute(context.Background(), tools.Invocation{}); !errors.Is(err, tools.ErrNotExecuted) {
		t.Fatalf("missing file err = %v", err)
	}
	path := filepath.Join(t.TempDir(), "h.json")
	os.WriteFile(path, []byte(`{"as_of":"2026-11-21T10:00:00Z","source":"manual","base_currency":"USD","positions":[{"account":"a","symbol":"X","currency":"USD","quantity":"2","price":"5"}]}`), 0o600)
	tool.Path = path
	res, err := tool.Execute(context.Background(), tools.Invocation{})
	if err != nil || !strings.Contains(string(res.Output), `"total_value_in_base":"10.00"`) {
		t.Fatalf("result = %s, %v", res.Output, err)
	}
}
