package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/xconnio/xconn-go"
)

const ProcedureGitStatus = "io.xconn.deskconn.deskconnd.git.status"
const ProcedureGitOriginal = "io.xconn.deskconn.deskconnd.git.original"

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

// ignoredEntries lists each gitignored file under repoRoot, matching `git
// status --ignored=matching`.
// ponytail: full filesystem walk, slow on a huge ignored tree (node_modules).
func ignoredEntries(repoRoot string, st git.Status, matcher gitignore.Matcher) ([]GitStatusEntry, error) {
	var entries []GitStatusEntry
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == repoRoot {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if _, tracked := st[relSlash]; tracked {
			return nil
		}
		if matcher.Match(strings.Split(relSlash, "/"), false) {
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

	var entries []GitStatusEntry
	for relSlash, fileStatus := range st {
		status, ok := classifyStatus(fileStatus)
		if !ok {
			continue
		}
		entries = append(entries, GitStatusEntry{
			Path:   filepath.Join(repoRoot, filepath.FromSlash(relSlash)),
			Status: status,
		})
	}

	ignored, err := ignoredEntries(repoRoot, st, matcher)
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

	if fileStatus, tracked := st[relSlash]; tracked {
		if fileStatus.Staging == git.Untracked {
			return &GitOriginalResult{IsRepo: true, IsNew: true, Untracked: true}, nil
		}
	} else if matcher.Match(strings.Split(relSlash, "/"), false) {
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

func (d *Deskconn) handleGitStatus(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	plaintext, err := DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	result, err := d.files.GitStatus(args.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	encryptedResult, err := EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(encryptedResult)
}

func (d *Deskconn) handleGitOriginal(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	plaintext, err := DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	result, err := d.files.GitOriginal(args.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	encryptedResult, err := EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(encryptedResult)
}
