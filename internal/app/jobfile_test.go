package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/skills"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

const examples = "../../docs/examples"

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(examples, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parse(t *testing.T, name string) app.Spec {
	t.Helper()
	s, err := app.ParseJob(name, read(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

func newApp(t *testing.T) *app.App {
	t.Helper()
	db := testutil.Open(t, testutil.DBPath(t))
	a := app.New(db, scripted.Provider(func(model.Request) model.Message { return model.Message{} }), "scripted", tools.Registry{}, testutil.NewClock().Now)
	if err := skills.LoadDir(a.Skills, filepath.Join(examples, "skills")); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMarkdownAndJSONTwinsAreTheSameSpec(t *testing.T) {
	for _, twin := range []string{"portfolio-intelligence", "first-job"} {
		md, js := parse(t, twin+".md"), parse(t, twin+".json")
		if !reflect.DeepEqual(md, js) {
			a, _ := json.Marshal(md)
			b, _ := json.Marshal(js)
			t.Errorf("%s twins differ:\n md   %s\n json %s", twin, a, b)
		}
	}
}

func TestEveryExampleCreates(t *testing.T) {
	a := newApp(t)
	files, _ := filepath.Glob(filepath.Join(examples, "*.md"))
	more, _ := filepath.Glob(filepath.Join(examples, "*.json"))
	for _, f := range append(files, more...) {
		if strings.HasSuffix(f, "holdings.json") || strings.HasSuffix(f, "mcp.json") {
			continue
		}
		if err := a.Check(parse(t, filepath.Base(f))); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
}

func TestExportThenCreateGivesTheSameAssignment(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	for _, name := range []string{"portfolio-intelligence.json", "airline-refund.md", "first-job.md"} {
		id, err := a.Create(ctx, parse(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		first, err := a.Export(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		md, err := first.Markdown()
		if err != nil {
			t.Fatal(err)
		}
		again, err := app.ParseMarkdown([]byte(md))
		if err != nil {
			t.Fatalf("%s: exported file does not parse: %v\n%s", name, err, md)
		}
		id2, err := a.Create(ctx, again)
		if err != nil {
			t.Fatal(err)
		}
		second, _ := a.Export(ctx, id2)
		if !reflect.DeepEqual(first, second) {
			x, _ := json.Marshal(first)
			y, _ := json.Marshal(second)
			t.Errorf("%s round trip differs:\n%s\n%s", name, x, y)
		}
	}
}

func TestBadJobFilesNameTheLine(t *testing.T) {
	ok := "---\nkind: project\nname: x\npolicy:\n  tools:\n    notify: allow\n  limits:\n    max_llm_spend_usd: 1\ncompletion:\n  criteria:\n    - description: done\n---\n\nDo it.\n"
	if _, err := app.ParseMarkdown([]byte(ok)); err != nil {
		t.Fatalf("valid file: %v", err)
	}
	for name, tc := range map[string]struct{ file, want string }{
		"unknown key":     {strings.Replace(ok, "name: x", "nmae: x", 1), `line 3: unknown key "nmae"`},
		"duplicate key":   {strings.Replace(ok, "name: x", "name: x\nname: y", 1), `line 4: key "name" appears twice`},
		"unknown mode":    {strings.Replace(ok, "notify: allow", "notify: approve", 1), `line 6: tool "notify": mode "approve"`},
		"body and key":    {strings.Replace(ok, "name: x", "name: x\nobjective: also", 1), "line 4: the objective is the text after the header"},
		"no closing line": {strings.Replace(ok, "---\n\nDo it.", "\nDo it.", 1), "line 1: the header has no closing --- line"},
		"alias":           {strings.Replace(ok, "name: x", "name: &n x\nkindx: *n", 1), "line 3: anchors"},
		"second document": {strings.Replace(ok, "kind: project\n", "kind: project\n--- {name: other}\n", 1), "line 3: only one header document"},
		"wrong type":      {strings.Replace(ok, "max_llm_spend_usd: 1", "max_llm_spend_usd: lots", 1), "line 8: policy.limits.max_llm_spend_usd has the wrong type"},
		"custom tag":      {strings.Replace(ok, "name: x", "name: !secret x", 1), "line 3: tags"},
		"merge key":       {strings.Replace(ok, "name: x", "name: x\n<<: {kind: project}", 1), "line 4: only plain keys"},
	} {
		_, err := app.ParseMarkdown([]byte(tc.file))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, assignments.ErrInvalid) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestBodyMayContainDashes(t *testing.T) {
	s, err := app.ParseMarkdown([]byte("---\nkind: project\nname: x\n---\nFirst part.\n---\nSecond part.\n"))
	if err != nil || s.Objective != "First part.\n---\nSecond part." {
		t.Fatalf("objective = %q, %v", s.Objective, err)
	}
}

func TestMisspeltJSONKeyIsRejected(t *testing.T) {
	_, err := app.ParseJSON([]byte(`{"kind":"project","name":"x","objectve":"typo"}`))
	if err == nil || !strings.Contains(err.Error(), "objectve") {
		t.Fatalf("err = %v", err)
	}
	if _, err := app.ParseJob("job.yaml", nil); err == nil {
		t.Fatal("an unknown extension was accepted")
	}
}

func TestCheckCreatesNothing(t *testing.T) {
	a := newApp(t)
	if err := a.Check(parse(t, "airline-refund.md")); err != nil {
		t.Fatal(err)
	}
	var n int
	a.DB.QueryRow(`SELECT COUNT(*) FROM assignments WHERE builtin IS NULL`).Scan(&n)
	if n != 0 {
		t.Fatalf("a dry run created %d assignments", n)
	}
}

func TestPolicyFiles(t *testing.T) {
	p, err := app.ParsePolicy("p.md", []byte("---\npolicy:\n  tools:\n    notify: allow\n  limits:\n    max_llm_spend_usd: 2\n---\n"))
	if err != nil || p.Tools["notify"].Mode != "allow" || p.Limits.MaxLLMSpendUSD != 2 {
		t.Fatalf("policy = %+v, %v", p, err)
	}
	if _, err := app.ParsePolicy("p.md", []byte("---\npolicy:\n  tools: {}\nname: x\n---\n")); err == nil {
		t.Fatal("a policy file with other keys was accepted")
	}
}

func FuzzParseMarkdown(f *testing.F) {
	for _, name := range []string{"first-job.md", "airline-refund.md", "portfolio-intelligence.md"} {
		b, _ := os.ReadFile(filepath.Join(examples, name))
		f.Add(b)
	}
	f.Add([]byte("---\nkind: project\n---\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := app.ParseMarkdown(data)
		if err != nil {
			return
		}
		md, err := s.Markdown()
		if err != nil {
			t.Fatalf("accepted spec does not render: %v", err)
		}
		again, err := app.ParseMarkdown([]byte(md))
		if err != nil {
			t.Fatalf("rendered spec does not parse: %v\n%s", err, md)
		}
		x, _ := json.Marshal(s)
		y, _ := json.Marshal(again)
		if string(x) != string(y) {
			t.Fatalf("render changed the spec:\n%s\n%s", x, y)
		}
	})
}
