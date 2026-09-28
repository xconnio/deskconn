package common

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type AISessionFile struct {
	Tool    string
	Path    string
	ModTime time.Time
	Size    int64
}

// ClaudeProjectDir returns the directory Claude Code itself would use under
// ~/.claude/projects/ for the project at path, relative to homeDir: Claude Code encodes a
// project's full absolute cwd (every "/" replaced with "-"), so that absolute path is
// reconstructed here as homeDir+path before encoding. Recomputing this from (homeDir, path)
// independently on each machine - rather than reusing a value computed elsewhere - is what
// lets the same project match regardless of username or home directory location.
func ClaudeProjectDir(homeDir, path string) string {
	return strings.ReplaceAll(filepath.Join(homeDir, path), string(filepath.Separator), "-")
}

// DiscoverClaudeSessions finds Claude Code session files for the project at path, given
// relative to homeDir (not as an absolute path). A missing project directory is not an error -
// it just means no sessions exist yet.
func DiscoverClaudeSessions(homeDir, path string) ([]AISessionFile, error) {
	dir := filepath.Join(homeDir, ".claude", "projects", ClaudeProjectDir(homeDir, path))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []AISessionFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		sessions = append(sessions, AISessionFile{
			Tool:    AIToolClaude,
			Path:    filepath.Join(dir, entry.Name()),
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
	}
	sortByModTimeDesc(sessions)
	return sessions, nil
}

func sortByModTimeDesc(sessions []AISessionFile) {
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})
}
