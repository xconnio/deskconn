package userd

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go"
)

// GitStatusEntry Status is one of "untracked", "added", "modified", "ignored".
// Anything else (clean, renamed, ...) is omitted from GitStatusResult.Entries.
type GitStatusEntry struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type GitStatusResult struct {
	IsRepo   bool             `json:"is_repo"`
	RepoRoot string           `json:"repo_root,omitempty"`
	Entries  []GitStatusEntry `json:"entries,omitempty"`
}

type GitOriginalResult struct {
	IsRepo    bool   `json:"is_repo"`
	IsNew     bool   `json:"is_new"`
	Untracked bool   `json:"untracked"`
	Ignored   bool   `json:"ignored"`
	Content   string `json:"content"`
}

// openRepo walks up from path for the enclosing repo, like `git rev-parse
// --show-toplevel`. A stray/incomplete `.git` fails to open, same as real git.
func openRepo(path string) (repo *git.Repository, repoRoot string, ok bool) {
	dir := path
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		dir = filepath.Dir(path)
	}
	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, "", false
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, "", false
	}
	return repo, wt.Filesystem.Root(), true
}

// repoStatus returns the worktree status (excludes gitignored paths, like
// plain `git status`) plus a matcher for the same patterns, so callers can
// separately test whether a path absent from the status is ignored.
func repoStatus(repo *git.Repository) (git.Status, gitignore.Matcher, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return nil, nil, err
	}
	st, err := wt.Status()
	if err != nil {
		return nil, nil, err
	}
	patterns, err := gitignore.ReadPatterns(wt.Filesystem, nil)
	if err != nil {
		return nil, nil, err
	}
	return st, gitignore.NewMatcher(append(patterns, wt.Excludes...)), nil
}

// trackedPaths returns every path in the index (files) and every directory
// holding at least one of them (dirs; "." is never added). A directory not in
// dirs has no tracked content, so it can be reported as a single entry.
func trackedPaths(repo *git.Repository) (files, dirs map[string]bool, err error) {
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, nil, err
	}
	files = make(map[string]bool, len(idx.Entries))
	dirs = make(map[string]bool)
	for _, e := range idx.Entries {
		files[e.Name] = true
		for d := path.Dir(e.Name); d != "." && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	return files, dirs, nil
}

// topUntrackedDir returns the highest ancestor of relSlash with no tracked
// content, like `git status`'s "?? build/", or "" if its parent is tracked.
// dirs is closed upward, so the climb stops at the first tracked dir.
func topUntrackedDir(relSlash string, dirs map[string]bool) string {
	top := ""
	for d := path.Dir(relSlash); d != "." && !dirs[d]; d = path.Dir(d) {
		top = d
	}
	return top
}

func classifyStatus(fs *git.FileStatus) (string, bool) {
	switch {
	case fs.Staging == git.Untracked:
		return "untracked", true
	case fs.Staging == git.Added:
		return "added", true
	case fs.Staging == git.Modified || fs.Worktree == git.Modified:
		return "modified", true
	default:
		return "", false
	}
}

// ignoredEntries lists gitignored paths under repoRoot, matching plain `git
// status --ignored`: an ignored directory is one entry, not one per file inside
// it, so a big node_modules doesn't blow past the 16MB WAMP message limit. A
// matching directory that still holds tracked (force-added) files is walked
// into instead, so those files aren't mislabeled.
func ignoredEntries(repoRoot string, files, dirs map[string]bool, matcher gitignore.Matcher) ([]GitStatusEntry, error) {
	var entries []GitStatusEntry
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == repoRoot {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if d.IsDir() {
			if !dirs[relSlash] && matcher.Match(strings.Split(relSlash, "/"), true) {
				entries = append(entries, GitStatusEntry{Path: p, Status: "ignored"})
				return fs.SkipDir
			}
			return nil
		}
		if !files[relSlash] && matcher.Match(strings.Split(relSlash, "/"), false) {
			entries = append(entries, GitStatusEntry{Path: p, Status: "ignored"})
		}
		return nil
	})
	return entries, err
}

func (f *FileBrowser) GitStatus(pathArg string) (*GitStatusResult, error) {
	resolved, err := resolvePath(pathArg)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(resolved); err != nil {
		return nil, err
	}

	repo, repoRoot, ok := openRepo(resolved)
	if !ok {
		return &GitStatusResult{IsRepo: false}, nil
	}

	st, matcher, err := repoStatus(repo)
	if err != nil {
		return nil, err
	}

	files, dirs, err := trackedPaths(repo)
	if err != nil {
		return nil, err
	}

	var entries []GitStatusEntry
	untrackedDirs := map[string]bool{}
	for relSlash, fileStatus := range st {
		status, ok := classifyStatus(fileStatus)
		if !ok {
			continue
		}
		// A wholly untracked folder is one entry, not one per file inside it.
		if status == "untracked" {
			if top := topUntrackedDir(relSlash, dirs); top != "" {
				if untrackedDirs[top] {
					continue
				}
				untrackedDirs[top] = true
				relSlash = top
			}
		}
		entries = append(entries, GitStatusEntry{
			Path:   filepath.Join(repoRoot, filepath.FromSlash(relSlash)),
			Status: status,
		})
	}

	ignored, err := ignoredEntries(repoRoot, files, dirs, matcher)
	if err != nil {
		return nil, err
	}
	entries = append(entries, ignored...)

	return &GitStatusResult{
		IsRepo:   true,
		RepoRoot: repoRoot,
		Entries:  entries,
	}, nil
}

func (f *FileBrowser) GitOriginal(pathArg string) (*GitOriginalResult, error) {
	resolved, err := resolvePath(pathArg)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(resolved); err != nil {
		return nil, err
	}

	repo, repoRoot, ok := openRepo(resolved)
	if !ok {
		return &GitOriginalResult{IsRepo: false, IsNew: true}, nil
	}

	rel, err := filepath.Rel(repoRoot, resolved)
	if err != nil {
		//nolint:nilerr // resolved is always under repoRoot, so this can't actually fail
		return &GitOriginalResult{IsRepo: true, IsNew: true}, nil
	}
	relSlash := filepath.ToSlash(rel)

	st, matcher, err := repoStatus(repo)
	if err != nil {
		return nil, err
	}

	files, _, err := trackedPaths(repo)
	if err != nil {
		return nil, err
	}

	if fileStatus, changed := st[relSlash]; changed && fileStatus.Staging == git.Untracked {
		return &GitOriginalResult{IsRepo: true, IsNew: true, Untracked: true}, nil
	} else if !changed && !files[relSlash] && matcher.Match(strings.Split(relSlash, "/"), false) {
		return &GitOriginalResult{IsRepo: true, IsNew: true, Ignored: true}, nil
	}

	content, ok := headContent(repo, relSlash)
	if !ok {
		//nolint:nilerr // no committed baseline (staged-but-uncommitted, repo has no commits yet, ...)
		return &GitOriginalResult{IsRepo: true, IsNew: true}, nil
	}

	return &GitOriginalResult{IsRepo: true, IsNew: false, Content: content}, nil
}

// headContent returns relSlash's content as of HEAD, or ok=false if absent.
func headContent(repo *git.Repository, relSlash string) (string, bool) {
	head, err := repo.Head()
	if err != nil {
		return "", false
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return "", false
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", false
	}
	file, err := tree.File(relSlash)
	if err != nil {
		return "", false
	}
	content, err := file.Contents()
	if err != nil {
		return "", false
	}
	return content, true
}

func (d *Deskconn) handleGitStatus(_ context.Context, inv *xconn.Invocation) *xconn.Result {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewResultError(common.ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	plaintext, err := common.DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}

	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}

	result, err := d.files.GitStatus(args.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	encryptedResult, err := common.EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}

	return xconn.NewResult(encryptedResult)
}

func (d *Deskconn) handleGitOriginal(_ context.Context, inv *xconn.Invocation) *xconn.Result {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewResultError(common.ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	plaintext, err := common.DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}

	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}

	result, err := d.files.GitOriginal(args.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	encryptedResult, err := common.EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}

	return xconn.NewResult(encryptedResult)
}
