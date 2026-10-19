package common

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// AgentSessionFile is one agent session's file on this machine.
type AgentSessionFile struct {
	Tool    string
	ID      string
	Path    string
	ModTime time.Time
	Size    int64
}

// DiscoverAgentSessions finds tool's session files for the project at path, relative to
// homeDir or absolute, newest first.
func DiscoverAgentSessions(tool, homeDir, path string) ([]AgentSessionFile, error) {
	switch tool {
	case ClaudeTool:
		return DiscoverClaudeSessions(homeDir, path)
	case CodexTool:
		return DiscoverCodexSessions(homeDir, path)
	}
	return nil, fmt.Errorf("unknown agent %q", tool)
}

// DiscoverAgentProjects lists the directories with tool's sessions on this machine, most
// recently used first.
func DiscoverAgentProjects(tool, homeDir string) ([]AgentProject, error) {
	switch tool {
	case ClaudeTool:
		return DiscoverClaudeProjects(homeDir)
	case CodexTool:
		return DiscoverCodexProjects(homeDir)
	}
	return nil, fmt.Errorf("unknown agent %q", tool)
}

// SummarizeAgentSessions turns discovered session files into the summaries `agent ls` and the
// web app show.
func SummarizeAgentSessions(sessions []AgentSessionFile) []AgentSessionSummary {
	var summaries []AgentSessionSummary
	for _, s := range sessions {
		var title string
		var messages []AgentMessage
		switch s.Tool {
		case ClaudeTool:
			_, title, messages = readClaudeSessionTail(s.Path)
			if title == "" {
				title = ClaudeSessionTitle(s.Path)
			}
		case CodexTool:
			messages = readCodexSessionTail(s.Path)
			title = codexSessionTitle(s.Path)
		}
		summaries = append(summaries, AgentSessionSummary{
			Tool:      s.Tool,
			SessionID: s.ID,
			Title:     truncateTitle(title),
			UpdatedAt: s.ModTime,
			Size:      s.Size,
			Messages:  messages,
		})
	}
	return summaries
}

// AgentSessionDir returns where a pulled session file called name goes on this machine, for
// the project at path (relative to homeDir).
func AgentSessionDir(tool, homeDir, path, name string) (string, error) {
	switch tool {
	case ClaudeTool:
		return filepath.Join(homeDir, ".claude", "projects", ClaudeProjectDir(homeDir, path)), nil
	case CodexTool:
		// Codex keeps sessions by day, as rollout-YYYY-MM-DDThh-mm-ss-<id>.jsonl.
		date, ok := strings.CutPrefix(name, "rollout-")
		if !ok || len(date) < len("2006-01-02") {
			return "", fmt.Errorf("unexpected codex session file name %q", name)
		}
		day, err := time.Parse("2006-01-02", date[:len("2006-01-02")])
		if err != nil {
			return "", fmt.Errorf("unexpected codex session file name %q", name)
		}
		return filepath.Join(codexSessionsRoot(homeDir), day.Format("2006/01/02")), nil
	}
	return "", fmt.Errorf("unknown agent %q", tool)
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
	return claudeDirUnsafe.ReplaceAllString(absProjectPath(homeDir, path), "-")
}

// absProjectPath is path, relative to homeDir or absolute, as an absolute path.
func absProjectPath(homeDir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(homeDir, path)
}

// DiscoverClaudeSessions finds Claude Code session files for the project at path, relative
// to homeDir or absolute (see ClaudeProjectDir). A missing project directory is not an
// error - it just means no sessions exist yet.
func DiscoverClaudeSessions(homeDir, path string) ([]AgentSessionFile, error) {
	return ClaudeSessionsIn(filepath.Join(homeDir, ".claude", "projects", ClaudeProjectDir(homeDir, path)))
}

// ClaudeSessionsIn returns the Claude Code session files in dir, one of ~/.claude/projects/'s
// entries, newest first. A missing dir has none.
func ClaudeSessionsIn(dir string) ([]AgentSessionFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []AgentSessionFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		sessions = append(sessions, AgentSessionFile{
			Tool:    ClaudeTool,
			ID:      strings.TrimSuffix(entry.Name(), ".jsonl"),
			Path:    filepath.Join(dir, entry.Name()),
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
	}
	sortByModTimeDesc(sessions)
	return sessions, nil
}

func sortByModTimeDesc(sessions []AgentSessionFile) {
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})
}

// DiscoverClaudeProjects lists the directories with Claude Code sessions on this machine,
// most recently used first.
func DiscoverClaudeProjects(homeDir string) ([]AgentProject, error) {
	root := filepath.Join(homeDir, ".claude", "projects")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return []AgentProject{}, nil
	}
	if err != nil {
		return nil, err
	}

	projects := []AgentProject{}
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
				projects = append(projects, AgentProject{Path: cwd, Sessions: len(sessions), UpdatedAt: sessions[0].ModTime})
				break
			}
		}
	}
	sortProjects(projects)
	return projects, nil
}

func sortProjects(projects []AgentProject) {
	slices.SortFunc(projects, func(a, b AgentProject) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
}

// readClaudeSessionTail reads the end of a session file: its cwd, its latest AI title and
// its last chat messages, skipping tool calls, thinking and sidechains (subagents).
func readClaudeSessionTail(path string) (cwd, title string, messages []AgentMessage) {
	readTailLines(path, func(line []byte) {
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
		if json.Unmarshal(line, &rec) != nil {
			return
		}
		switch {
		case rec.Type == "ai-title" && rec.AITitle != "":
			title = rec.AITitle
		case (rec.Type == AgentRoleUser || rec.Type == agentRoleAssistant) && !rec.IsSidechain && !rec.IsMeta:
			if rec.CWD != "" {
				cwd = rec.CWD
			}
			if text := agentMessageText(rec.Message.Content); text != "" {
				messages = append(messages, AgentMessage{Role: rec.Type, Text: text})
			}
		}
	})
	return cwd, title, recentMessages(messages)
}

// codexSessionsRoot is where Codex keeps its sessions, by day: YYYY/MM/DD/rollout-*.jsonl.
func codexSessionsRoot(homeDir string) string {
	return filepath.Join(homeDir, ".codex", "sessions")
}

// codexSessions returns every Codex session file on this machine, newest first, with the
// directory each ran in.
func codexSessions(homeDir string) ([]AgentSessionFile, []string, error) {
	var files []AgentSessionFile
	err := filepath.WalkDir(codexSessionsRoot(homeDir), func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil //nolint:nilerr // a session that vanished mid-walk is skipped
		}
		stem := strings.TrimSuffix(name, ".jsonl")
		files = append(files, AgentSessionFile{
			Tool:    CodexTool,
			ID:      stem[max(len(stem)-codexIDLen, 0):],
			Path:    path,
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sortByModTimeDesc(files)

	cwds := make([]string, len(files))
	for i, f := range files {
		cwds[i] = codexSessionCWD(f.Path)
	}
	return files, cwds, nil
}

// codexIDLen is the length of the UUID ending a Codex session file's name.
const codexIDLen = 36

// DiscoverCodexSessions finds the Codex session files that ran in the project at path,
// relative to homeDir or absolute, newest first.
func DiscoverCodexSessions(homeDir, path string) ([]AgentSessionFile, error) {
	files, cwds, err := codexSessions(homeDir)
	if err != nil {
		return nil, err
	}
	dir := absProjectPath(homeDir, path)
	var sessions []AgentSessionFile
	for i, f := range files {
		if cwds[i] == dir {
			sessions = append(sessions, f)
		}
	}
	return sessions, nil
}

// DiscoverCodexProjects lists the directories with Codex sessions on this machine, most
// recently used first.
func DiscoverCodexProjects(homeDir string) ([]AgentProject, error) {
	files, cwds, err := codexSessions(homeDir)
	if err != nil {
		return nil, err
	}
	byDir := map[string]*AgentProject{}
	projects := []AgentProject{}
	for i, f := range files {
		if cwds[i] == "" {
			continue
		}
		if p, ok := byDir[cwds[i]]; ok {
			p.Sessions++
			continue
		}
		// files are newest first, so a directory's first session is its latest.
		byDir[cwds[i]] = &AgentProject{Path: cwds[i], Sessions: 1, UpdatedAt: f.ModTime}
	}
	for _, p := range byDir {
		projects = append(projects, *p)
	}
	sortProjects(projects)
	return projects, nil
}

// codexSessionCWD returns the directory a Codex session ran in, from its first record.
func codexSessionCWD(path string) string {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck

	line, _ := bufio.NewReader(f).ReadBytes('\n')
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			CWD string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" {
		return ""
	}
	return meta.Payload.CWD
}

// codexMessage returns the role and text of a Codex record that's a user's or the
// assistant's chat message, or "" for any other record.
func codexMessage(line []byte) (role, text string) {
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "response_item" || rec.Payload.Type != "message" {
		return "", ""
	}
	if rec.Payload.Role != AgentRoleUser && rec.Payload.Role != agentRoleAssistant {
		return "", ""
	}
	return rec.Payload.Role, agentMessageText(rec.Payload.Content)
}

// readCodexSessionTail returns a Codex session's last chat messages.
func readCodexSessionTail(path string) []AgentMessage {
	var messages []AgentMessage
	readTailLines(path, func(line []byte) {
		if role, text := codexMessage(line); text != "" {
			messages = append(messages, AgentMessage{Role: role, Text: text})
		}
	})
	return recentMessages(messages)
}

// codexSessionTitle returns a Codex session's first prompt, which Codex's own resume picker
// shows: Codex doesn't title sessions.
func codexSessionTitle(path string) string {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck

	reader := bufio.NewReader(f)
	for range AgentTitleScanLines {
		line, err := reader.ReadBytes('\n')
		if role, text := codexMessage(line); role == AgentRoleUser && text != "" {
			return text
		}
		if err != nil {
			break
		}
	}
	return ""
}

// readTailLines calls fn with each line of the end of path: session files grow to many MBs,
// so only the last AgentSessionTailBytes are read.
func readTailLines(path string, fn func(line []byte)) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil {
		return
	}
	offset := max(info.Size()-AgentSessionTailBytes, 0)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return
	}

	reader := bufio.NewReader(f)
	if offset > 0 {
		// Starting mid-file: drop the partial first line.
		if _, err := reader.ReadBytes('\n'); err != nil {
			return
		}
	}
	for {
		line, err := reader.ReadBytes('\n')
		fn(line)
		if err != nil {
			return
		}
	}
}

// recentMessages returns messages' last AgentRecentMessages, keeping the latest prompt, which
// tells sessions apart best, even past a long reply.
func recentMessages(messages []AgentMessage) []AgentMessage {
	if len(messages) <= AgentRecentMessages {
		return messages
	}
	cut := len(messages) - AgentRecentMessages
	recent := messages[cut:]
	if !slices.ContainsFunc(recent, func(m AgentMessage) bool { return m.Role == AgentRoleUser }) {
		for i := cut - 1; i >= 0; i-- {
			if messages[i].Role == AgentRoleUser {
				return append([]AgentMessage{messages[i]}, recent[1:]...)
			}
		}
	}
	return recent
}

// agentMessageText returns a message's text: the content itself, or its text blocks (Claude's
// "text", Codex's "input_text" and "output_text").
// ponytail: text starting with "<" is taken for the agent's injected command/system/context
// markup, so a user message that really starts with "<" isn't shown.
func agentMessageText(content json.RawMessage) string {
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
			if b.Type == "text" || strings.HasSuffix(b.Type, "_text") {
				parts = append(parts, b.Text)
			}
		}
		text = strings.Join(parts, "\n")
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<") {
		return ""
	}
	if r := []rune(text); len(r) > AgentMaxMessageLen {
		text = string(r[:AgentMaxMessageLen]) + "…"
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
	for i := 0; i < AgentTitleScanLines && scanner.Scan(); i++ {
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
			return line.Summary
		}
		if firstUserText == "" && line.Type == AgentRoleUser && line.Message.Role == AgentRoleUser {
			if text, ok := line.Message.Content.(string); ok && text != "" {
				firstUserText = text
			}
		}
	}
	return firstUserText
}

func truncateTitle(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > AgentMaxTitleLen {
		return s[:AgentMaxTitleLen] + "…"
	}
	return s
}
