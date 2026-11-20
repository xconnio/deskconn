package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func fixBashCompletionCommands(script string) string {
	const marker = "complete -F _desk_bash_autocomplete -o default desk"
	return strings.Replace(script, marker, marker+" deskconn", 1)
}

// fixBashCompletionWordBreaks clears ':' from COMP_WORDBREAKS so bash
// completes "machine:path" as one word instead of splitting at the colon.
func fixBashCompletionWordBreaks(script string) string {
	const marker = "_desk_bash_autocomplete() {\n"
	return strings.Replace(script, marker, marker+"    COMP_WORDBREAKS=${COMP_WORDBREAKS//:}\n", 1)
}

// fixBashCompletionSpacing makes path completions behave like "cd": the menu shows only the last
// path segment (-o filenames), and no trailing space is added after directories/"machine:" prefixes.
// Leaf files are left to bash's default space-append.
func fixBashCompletionSpacing(script string) string {
	const marker = `    COMPREPLY=( $(compgen -W "${opts}" -- ${cur}) )` + "\n"
	const replacement = marker +
		"    compopt -o filenames 2>/dev/null || true\n" +
		`    if [ "${#COMPREPLY[@]}" -eq 1 ] && [[ "${COMPREPLY[0]}" == */ || "${COMPREPLY[0]}" == *: ]]; then` + "\n" +
		"        compopt -o nospace 2>/dev/null || true\n" +
		"    fi\n"
	return strings.Replace(script, marker, replacement, 1)
}

func fixZshCompletion(script string) string {
	script = strings.Replace(script, "#compdef desk", "#compdef desk deskconn", 1)

	const oldBody = "    local matches=($(${words[1]} --completion-bash \"${(@)words[2,$CURRENT]}\"))\n" +
		"    compadd -a matches"
	const newBody = `    local -a matches
    local -a lines
    local line m
    lines=("${(@f)$(${words[1]} --completion-bash "${(@)words[2,$CURRENT]}")}")
    for line in "${lines[@]}"; do
        [[ -n "$line" ]] && matches+=("$line")
    done

    if [[ ${#matches[@]} -gt 0 ]]; then
        local -a dirs_and_colons files_and_cmds
        for m in "${matches[@]}"; do
            if [[ "$m" == */ || "$m" == *: ]]; then
                dirs_and_colons+=("$m")
            else
                files_and_cmds+=("$m")
            fi
        done
        [[ ${#dirs_and_colons[@]} -gt 0 ]] && compadd -S '' -a dirs_and_colons
        [[ ${#files_and_cmds[@]} -gt 0 ]] && compadd -a files_and_cmds
    fi`

	script = strings.Replace(script, oldBody, newBody, 1)

	const oldFooter = `if [[ "$(basename -- ${(%):-%x})" != "_desk" ]]; then
    compdef _desk desk
fi`

	const newFooter = `if [[ "$(basename -- ${(%):-%x})" != "_desk" ]]; then
    compdef _desk desk deskconn 2>/dev/null || true
fi

if [[ "$funcstack[1]" = "_desk" ]]; then
    _desk "$@"
fi`

	return strings.Replace(script, oldFooter, newFooter, 1)
}

func linkOptionalCompletions(zshDir, bashDir string) {
	for _, dir := range []string{"/opt/homebrew/share/zsh/site-functions", "/usr/local/share/zsh/site-functions"} {
		if isWritableDirectory(dir) {
			trySymlink(filepath.Join(zshDir, "_desk"), filepath.Join(dir, "_desk"))
			removeSymlink(filepath.Join(dir, "_deskconn"))
		}
	}
	for _, dir := range []string{"/opt/homebrew/etc/bash_completion.d", "/usr/local/etc/bash_completion.d"} {
		if isWritableDirectory(dir) {
			trySymlink(filepath.Join(bashDir, "desk"), filepath.Join(dir, "desk"))
			trySymlink(filepath.Join(bashDir, "deskconn"), filepath.Join(dir, "deskconn"))
		}
	}
}

// trySymlink points link at target. An existing file at link is replaced only if it is a
// symlink, so a regular file another tool installed there is left alone. The new link is
// created under a temporary name and renamed into place, so a failure never leaves link missing.
func trySymlink(target, link string) {
	if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return
	} else if err != nil && !os.IsNotExist(err) {
		return
	}

	tmp := fmt.Sprintf("%s.tmp-%d", link, os.Getpid())
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
	}
}

// removeSymlink removes path only if it is a symlink, leaving files other tools own alone.
func removeSymlink(path string) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(path)
	}
}

func isWritableDirectory(path string) bool {
	testFile := filepath.Join(path, fmt.Sprintf(".deskconn-write-test-%d", os.Getpid()))
	if err := os.WriteFile(testFile, []byte(""), 0644); err != nil { // nolint: gosec
		return false
	}
	_ = os.Remove(testFile)
	return true
}
