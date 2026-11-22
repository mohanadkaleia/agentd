package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"agentd/internal/app"
	"agentd/internal/policy"
	"agentd/internal/runtime"
	"agentd/internal/system"
)

// catalogueEntry is one configured model: its spec and an optional alias.
type catalogueEntry struct {
	alias, spec string
}

// parseCatalogue reads -models ("alias=spec" or "spec", comma-separated, the first is the
// default), falling back to -model and -fallback-models so existing setups do not change.
func parseCatalogue(models, model, fallbacks string) ([]catalogueEntry, error) {
	var out []catalogueEntry
	seen := map[string]bool{}
	add := func(raw string) error {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil
		}
		e := catalogueEntry{spec: raw}
		if alias, spec, ok := strings.Cut(raw, "="); ok {
			e = catalogueEntry{alias: strings.TrimSpace(alias), spec: strings.TrimSpace(spec)}
			if slices.Contains(policy.Tiers, e.alias) {
				return fmt.Errorf("model alias %q is a tier name; map tiers with -tier", e.alias)
			}
		}
		if seen[e.spec] {
			return nil
		}
		seen[e.spec] = true
		out = append(out, e)
		return nil
	}
	if models != "" {
		for _, m := range strings.Split(models, ",") {
			if err := add(m); err != nil {
				return nil, err
			}
		}
	} else if err := add(model); err != nil {
		return nil, err
	}
	for _, m := range strings.Split(fallbacks, ",") {
		if err := add(m); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// tierFlags collects -tier fast=<model> mappings.
type tierFlags map[string]string

func (t tierFlags) String() string { return "" }

func (t tierFlags) Set(v string) error {
	for _, pair := range strings.Split(v, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		tier, target, ok := strings.Cut(pair, "=")
		tier = strings.TrimSpace(tier)
		if !ok || !slices.Contains(policy.Tiers, tier) || strings.TrimSpace(target) == "" {
			return fmt.Errorf("-tier wants fast=, default= or strong= and a model, got %q", pair)
		}
		t[tier] = strings.TrimSpace(target)
	}
	return nil
}

// configureModels installs the catalogue and tiers on the runtime, creating a provider for
// each model. Every tier must name a configured model.
func configureModels(a *app.App, entries []catalogueEntry, tiers tierFlags, effort string) error {
	if len(entries) == 0 {
		return nil
	}
	rt := a.Runtime
	for i, e := range entries {
		p, err := newProvider(e.spec, effort)
		if err != nil {
			return fmt.Errorf("model %q: %w", e.spec, err)
		}
		if i == 0 {
			rt.Model, rt.ModelID, rt.ModelAlias, rt.ModelLocal = p, e.spec, e.alias, isLocal(e.spec)
			continue
		}
		rt.Fallbacks = append(rt.Fallbacks, runtime.Candidate{ID: e.spec, Alias: e.alias, Provider: p, Local: isLocal(e.spec)})
	}
	rt.Tiers = map[string]string{}
	for tier, target := range tiers {
		if _, err := rt.Choice(target); err != nil || slices.Contains(policy.Tiers, target) {
			return fmt.Errorf("-tier %s=%s: not a configured model (use -models)", tier, target)
		}
		rt.Tiers[tier] = target
	}
	return nil
}

// adoptCatalogue gives a command run beside `agentd serve` the models serve has, so choices
// can be checked here; providers are not created.
func adoptCatalogue(ctx context.Context, a *app.App, db *sql.DB) {
	if a.Runtime.ModelID != "" {
		return
	}
	live, _, err := system.Alive(ctx, db, time.Now())
	if err != nil || len(live.Models) == 0 {
		return
	}
	rt := a.Runtime
	for i, m := range live.Models {
		if i == 0 {
			rt.ModelID, rt.ModelAlias, rt.ModelLocal = m.ID, m.Alias, m.Local
			continue
		}
		rt.Fallbacks = append(rt.Fallbacks, runtime.Candidate{ID: m.ID, Alias: m.Alias, Local: m.Local})
	}
	rt.Tiers = live.Tiers
}

func catalogueInfo(rt *runtime.Runtime) ([]system.ModelInfo, map[string]string) {
	var out []system.ModelInfo
	for _, c := range rt.Catalogue() {
		if c.ID != "" {
			out = append(out, system.ModelInfo{ID: c.ID, Alias: c.Alias, Local: c.Local})
		}
	}
	return out, rt.Tiers
}

// printModels lists the catalogue, the tiers and whether each model's credential is
// present (never its value).
func printModels(a *app.App) error {
	rt := a.Runtime
	if rt.ModelID == "" {
		return fmt.Errorf("no models configured: pass -model or -models (or run it next to a running agentd serve)")
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "MODEL\tALIAS\tWHERE\tCREDENTIAL\t")
	for i, c := range rt.Catalogue() {
		where := "cloud"
		if c.Local {
			where = "local"
		}
		if i == 0 {
			where += ", default"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n", c.ID, c.Alias, where, credential(c.ID))
	}
	tiers := make([]string, 0, len(rt.Tiers))
	for t := range rt.Tiers {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers)
	fmt.Fprintln(w, "\nTIER\tMODEL\t\t\t")
	for _, t := range policy.Tiers {
		target, ok := rt.Tiers[t]
		if !ok {
			target = "(the default)"
		}
		fmt.Fprintf(w, "%s\t%s\t\t\t\n", t, target)
	}
	return nil
}

func credential(spec string) string {
	name, _, _ := strings.Cut(spec, "/")
	switch name {
	case "anthropic":
		if os.Getenv("ANTHROPIC_API_KEY") != "" || keychainSecret("agentd", "anthropic-api-key") != "" {
			return "present"
		}
		return "absent"
	}
	return "not needed"
}
