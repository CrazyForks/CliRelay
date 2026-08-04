package thinking

import (
	"strconv"
	"strings"
)

// ExtractThinkingLevel returns the request body's thinking level or numeric budget
// using the same protocol-specific parsing as ApplyThinking.
func ExtractThinkingLevel(body []byte, format string) string {
	config := extractThinkingConfig(body, strings.ToLower(strings.TrimSpace(format)))
	if !hasThinkingConfig(config) {
		return ""
	}

	switch config.Mode {
	case ModeNone:
		return string(LevelNone)
	case ModeAuto:
		return string(LevelAuto)
	case ModeLevel:
		return strings.TrimSpace(string(config.Level))
	case ModeBudget:
		return strconv.Itoa(config.Budget)
	default:
		return ""
	}
}
