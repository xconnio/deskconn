package deskconnd

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"

	"github.com/xconnio/deskconn/common"
)

// AISessionTitle returns a best-effort human-readable title for a Claude Code session file - the
// same summary its own --resume picker shows, falling back to the first user message.
func AISessionTitle(path string) string {
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return ""
	}
	defer file.Close() //nolint:errcheck

	var firstUserText string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for i := 0; i < common.AITitleScanLines && scanner.Scan(); i++ {
		var line struct {
			Type    string `json:"type"`
			Summary string `json:"summary"`
			Message struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type == "summary" && line.Summary != "" {
			return truncateTitle(line.Summary)
		}
		if firstUserText == "" && line.Type == "user" && line.Message.Role == "user" {
			if text, ok := line.Message.Content.(string); ok && text != "" {
				firstUserText = text
			}
		}
	}
	return truncateTitle(firstUserText)
}

func truncateTitle(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > common.AIMaxTitleLen {
		return s[:common.AIMaxTitleLen] + "…"
	}
	return s
}
