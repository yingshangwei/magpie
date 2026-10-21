package gateway

import (
	"strings"

	"github.com/yetone/magpie/internal/provider"
)

// antigravityClaudeSystem adapts Claude Code's leading client metadata for
// this fork's Gemini 4 route. Antigravity rejects either marker on its own
// (#666); the neutral prompt on the same account succeeds. Other models
// and accounts keep their prompts. Messages, tools and business rules
// containing the same words are not rewritten.
func antigravityClaudeSystem(p provider.Provider, model, system string) string {
	if accountAgent(p) != "antigravity" || !strings.HasPrefix(model, "gemini-4-") {
		return system
	}
	// Parsed text blocks are adjacent, so the SDK block may immediately
	// follow the billing line's last semicolon, with no newline between.
	if line := billingHeader.FindString(system); strings.Contains(line, "=") {
		system = system[len(line):]
	}
	const identity = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	if strings.HasPrefix(system, identity) {
		system = "You are a helpful coding assistant." + system[len(identity):]
	}
	return system
}
