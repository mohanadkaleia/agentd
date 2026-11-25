package runtime

import (
	"embed"
	"strings"
	"text/template"

	"agentd/internal/assignments"
)

// The system prompts for work tasks live in prompts/: one per kind of assignment, both
// ending with common.md.
//
//go:embed prompts/*.md
var promptFiles embed.FS

var prompts = template.Must(template.ParseFS(promptFiles, "prompts/*.md"))

func systemPrompt(a assignments.Assignment) string {
	name := "project.md"
	if a.Kind == assignments.KindResponsibility {
		name = "responsibility.md"
	}
	var b strings.Builder
	if err := prompts.ExecuteTemplate(&b, name, map[string]string{
		"Name": a.Name, "Objective": a.Objective, "NotePrefix": NotePrefix,
	}); err != nil {
		panic(err) // the templates are embedded, and a test runs both
	}
	return strings.TrimRight(b.String(), "\n")
}
