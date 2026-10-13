package tools

import "strings"

func truncateToolText(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "..."
}

func normalizeDurationMinutes(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	if value > 1440 {
		return 1440
	}
	return value
}
