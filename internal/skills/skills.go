package skills

import (
	"fmt"
	"strings"
)

type Skill struct {
	Name         string
	Description  string
	Instructions string
	Tools        []string
}

type Registry map[string]Skill

func (r Registry) Add(s Skill) {
	r[s.Name] = s
}

func (r Registry) Resolve(names []string) ([]Skill, error) {
	out := make([]Skill, 0, len(names))
	for _, name := range names {
		s, ok := r[name]
		if !ok {
			return nil, fmt.Errorf("unknown skill %q", name)
		}
		out = append(out, s)
	}
	return out, nil
}

func Prompt(loaded []Skill) string {
	if len(loaded) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nSkills loaded for this task:")
	for _, s := range loaded {
		fmt.Fprintf(&b, "\n\n## %s\n%s", s.Name, s.Instructions)
		if len(s.Tools) > 0 {
			fmt.Fprintf(&b, "\nTools this skill uses: %s.", strings.Join(s.Tools, ", "))
		}
	}
	return b.String()
}
