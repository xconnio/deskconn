package common

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

type ClaudeSessionFile struct {
	Tool    string
	Path    string
	ModTime time.Time
	Size    int64
}

// claudeDirUnsafe is every character Claude Code replaces with "-" when naming a project's
// directory under ~/.claude/projects/.
var claudeDirUnsafe = regexp.MustCompile(`[^a-zA-Z0-9]`)

// ClaudeProjectDir returns the directory Claude Code itself would use under
// ~/.claude/projects/ for the project at path: Claude Code encodes a project's full absolute
// cwd (every character but letters and digits replaced with "-"). A relative path is taken
// relative to homeDir, which is what lets the same project match across machines regardless
// of username or home directory location; an absolute one is used as is.
func ClaudeProjectDir(homeDir, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(homeDir, path)
	}
	return claudeDirUnsafe.ReplaceAllString(path, "-")
}

// DiscoverClaudeSessions finds Claude Code session files for the project at path, relative
// to homeDir or absolute (see ClaudeProjectDir). A missing project directory is not an
// error - it just means no sessions exist yet.
func DiscoverClaudeSessions(homeDir, path string) ([]ClaudeSessionFile, error) {
	return ClaudeSessionsIn(filepath.Join(homeDir, ".claude", "projects", ClaudeProjectDir(homeDir, path)))
}

// ClaudeSessionsIn returns the Claude Code session files in dir, one of ~/.claude/projects/'s
// entries, newest first. A missing dir has none.
func ClaudeSessionsIn(dir string) ([]ClaudeSessionFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []ClaudeSessionFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		sessions = append(sessions, ClaudeSessionFile{
			Tool:    ClaudeTool,
			Path:    filepath.Join(dir, entry.Name()),
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
	}
	sortByModTimeDesc(sessions)
	return sessions, nil
}

func sortByModTimeDesc(sessions []ClaudeSessionFile) {
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})
}

// SummarizeClaudeSessions turns discovered session files into the summaries `agent ls` and the
// web app show.
func SummarizeClaudeSessions(sessions []ClaudeSessionFile) []ClaudeSessionSummary {
	var summaries []ClaudeSessionSummary
	for _, s := range sessions {
		_, title, messages := readClaudeSessionTail(s.Path)
		if title == "" {
			title = ClaudeSessionTitle(s.Path)
		} else {
			title = truncateTitle(title)
		}
		summaries = append(summaries, ClaudeSessionSummary{
			Tool:      s.Tool,
			SessionID: strings.TrimSuffix(filepath.Base(s.Path), ".jsonl"),
			Title:     title,
			UpdatedAt: s.ModTime,
			Size:      s.Size,
			Messages:  messages,
		})
	}
	return summaries
}

// DiscoverClaudeProjects lists the directories with Claude Code sessions on this machine,
// most recently used first.
func DiscoverClaudeProjects(homeDir string) ([]ClaudeProject, error) {
	root := filepath.Join(homeDir, ".claude", "projects")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return []ClaudeProject{}, nil
	}
	if err != nil {
		return nil, err
	}

	projects := []ClaudeProject{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sessions, err := ClaudeSessionsIn(filepath.Join(root, e.Name()))
		if err != nil || len(sessions) == 0 {
			continue
		}
		// The newest sessions say where the directory is: Claude Code's encoding of it into
		// the entry's name can't be reversed.
		for _, s := range sessions[:min(len(sessions), 5)] {
			if cwd, _, _ := readClaudeSessionTail(s.Path); cwd != "" {
				projects = append(projects, ClaudeProject{Path: cwd, Sessions: len(sessions), UpdatedAt: sessions[0].ModTime})
				break
			}
		}
	}
	slices.SortFunc(projects, func(a, b ClaudeProject) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return projects, nil
}

// readClaudeSessionTail reads the end of a session file: its cwd, its latest AI title and
// its last chat messages, skipping tool calls, thinking and sidechains (subagents). Session
// files grow to many MBs, so only the last ClaudeSessionTailBytes are read.
func readClaudeSessionTail(path string) (cwd, title string, messages []ClaudeMessage) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", "", nil
	}
	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil {
		return "", "", nil
	}
	offset := max(info.Size()-ClaudeSessionTailBytes, 0)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", "", nil
	}

	reader := bufio.NewReader(f)
	if offset > 0 {
		// Starting mid-file: drop the partial first line.
		if _, err := reader.ReadBytes('\n'); err != nil {
			return "", "", nil
		}
	}
	for {
		line, readErr := reader.ReadBytes('\n')
		var rec struct {
			Type        string `json:"type"`
			CWD         string `json:"cwd"`
			AITitle     string `json:"aiTitle"`
			IsSidechain bool   `json:"isSidechain"`
			IsMeta      bool   `json:"isMeta"`
			Message     struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) == nil {
			switch {
			case rec.Type == "ai-title" && rec.AITitle != "":
				title = rec.AITitle
			case (rec.Type == ClaudeRoleUser || rec.Type == "assistant") && !rec.IsSidechain && !rec.IsMeta:
				if rec.CWD != "" {
					cwd = rec.CWD
				}
				if text := claudeMessageText(rec.Message.Content); text != "" {
					messages = append(messages, ClaudeMessage{Role: rec.Type, Text: text})
				}
			}
		}
		if readErr != nil {
			break
		}
	}

	if len(messages) > ClaudeRecentMessages {
		cut := len(messages) - ClaudeRecentMessages
		recent := messages[cut:]
		// Keep the latest prompt, which tells sessions apart best, even past a long reply.
		if !slices.ContainsFunc(recent, func(m ClaudeMessage) bool { return m.Role == ClaudeRoleUser }) {
			for i := cut - 1; i >= 0; i-- {
				if messages[i].Role == ClaudeRoleUser {
					recent = append([]ClaudeMessage{messages[i]}, recent[1:]...)
					break
				}
			}
		}
		messages = recent
	}
	return cwd, title, messages
}

// claudeMessageText returns a message's text: the content itself, or its text blocks.
// ponytail: text starting with "<" is taken for Claude Code's injected command/system
// markup, so a user message that really starts with "<" isn't shown.
func claudeMessageText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(content, &blocks) != nil {
			return ""
		}
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		text = strings.Join(parts, "\n")
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<") {
		return ""
	}
	if r := []rune(text); len(r) > ClaudeMaxMessageLen {
		text = string(r[:ClaudeMaxMessageLen]) + "…"
	}
	return text
}

// ClaudeSessionTitle returns a best-effort human-readable title for a Claude Code session file - the
// same summary its own --resume picker shows, falling back to the first user message.
func ClaudeSessionTitle(path string) string {
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return ""
	}
	defer file.Close() //nolint:errcheck

	var firstUserText string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for i := 0; i < ClaudeTitleScanLines && scanner.Scan(); i++ {
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
	if len(s) > ClaudeMaxTitleLen {
		return s[:ClaudeMaxTitleLen] + "…"
	}
	return s
}
