package chat

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
)

// The chat's system prompt is two files. identity.md says who agentd is and how it talks;
// the owner can replace it. rules.md holds the safety rules and is always sent.
var (
	//go:embed prompts/identity.md
	defaultIdentity string
	//go:embed prompts/rules.md
	rules string
	// summaryPrompt instructs the model that keeps a long conversation's running summary.
	//go:embed prompts/summary.md
	summaryPrompt string
)

// IdentityFile is the file in agentd's data directory that replaces the default identity.
const IdentityFile = "identity.md"

const maxIdentityBytes = 16 << 10

// systemPrompt is read on every turn, so an edited identity.md applies from the next reply.
func (s *Service) systemPrompt() string {
	return strings.TrimSpace(s.identity()) + "\n" + strings.TrimSpace(rules)
}

func (s *Service) identity() string {
	if s.PromptDir == "" {
		return defaultIdentity
	}
	raw, err := os.ReadFile(filepath.Join(s.PromptDir, IdentityFile))
	if err != nil || len(raw) > maxIdentityBytes || strings.TrimSpace(string(raw)) == "" {
		return defaultIdentity
	}
	return string(raw)
}
