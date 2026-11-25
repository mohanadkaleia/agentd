package main

import (
	tea "github.com/charmbracelet/bubbletea"

	"strings"
	"unicode"

	"agentd/internal/runtime"
)

// modelPicker opens a dialog to choose this conversation's model, the way /model <choice>
// would. It reports false when serve's models are not known, so /model prints them instead.
func (t *tui) modelPicker() bool {
	a := t.c.app
	adoptCatalogue(t.ctx, a, a.DB)
	models := a.Runtime.Catalogue()
	if len(models) == 0 || models[0].ID == "" {
		return false
	}
	var current string
	a.DB.QueryRowContext(t.ctx, `SELECT COALESCE(model, '') FROM conversations WHERE id = ?`, t.c.conv).Scan(&current)

	d := &tuiDialog{title: "Select model", tint: t.theme.accent,
		sub: "Applies to this conversation from its next reply."}
	choose := func(value string) func() tea.Cmd {
		return func() tea.Cmd { return t.submit("/model " + value) }
	}
	auto := "agentd picks per message"
	if fast, ok := a.Runtime.Tiers["fast"]; ok {
		auto = modelName(a.Runtime, fast) + " for chat, bigger models for harder work"
	}
	d.opts = append(d.opts, tuiOption{label: mark("Automatic", current == ""), note: auto, run: choose("auto")})
	for _, m := range models {
		value := m.ID
		if m.Alias != "" {
			value = m.Alias
		}
		on := current != "" && (current == m.ID || current == m.Alias)
		if on {
			d.at = len(d.opts)
		}
		d.opts = append(d.opts, tuiOption{label: mark(prettyModel(m.ID), on), note: modelNote(m), run: choose(value)})
	}
	for _, tier := range []string{"fast", "default", "strong"} {
		if current == tier {
			d.opts = append(d.opts, tuiOption{label: mark("Tier "+tier, true), note: modelName(a.Runtime, a.Runtime.Tiers[tier]), run: choose(tier)})
			d.at = len(d.opts) - 1
		}
	}
	t.closeDialog()
	t.dialog = d
	return true
}

func mark(label string, on bool) string {
	if on {
		return label + " ✔"
	}
	return label
}

// modelName is the friendly name of a configured model, alias or id.
func modelName(r *runtime.Runtime, value string) string {
	for _, m := range r.Catalogue() {
		if m.ID == value || (m.Alias != "" && m.Alias == value) {
			return prettyModel(m.ID)
		}
	}
	return value
}

// prettyModel turns "anthropic/claude-sonnet-5-5" into "Sonnet 5.5"; other ids are kept.
func prettyModel(id string) string {
	provider, name, _ := strings.Cut(id, "/")
	if provider == "anthropic" && name == "" {
		return "Claude"
	}
	rest, ok := strings.CutPrefix(name, "claude-")
	if !ok {
		if name == "" {
			return id
		}
		return name
	}
	family, version, _ := strings.Cut(rest, "-")
	if family == "" {
		return id
	}
	r := []rune(family)
	r[0] = unicode.ToUpper(r[0])
	out := string(r)
	if version != "" {
		out += " " + strings.ReplaceAll(version, "-", ".")
	}
	return out
}

func modelNote(m runtime.Candidate) string {
	if m.Local {
		return "runs on this machine"
	}
	switch {
	case strings.Contains(m.ID, "opus"):
		return "most capable, for complex work"
	case strings.Contains(m.ID, "sonnet"):
		return "smart and fast, for everyday tasks"
	case strings.Contains(m.ID, "haiku"):
		return "fastest, for quick answers"
	case m.ID == "anthropic":
		return "Claude via the Anthropic API"
	}
	return "cloud"
}
