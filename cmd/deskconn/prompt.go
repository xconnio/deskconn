package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// confirmPrompt prints prompt, reads a line from stdin, and reports whether the response counts
// as affirmative: "y"/"yes" (case-insensitive) are always affirmative, and an empty response
// (just Enter) falls back to defaultYes.
func confirmPrompt(prompt string, defaultYes bool) (bool, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	text, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "y", "yes":
		return true, nil
	case "":
		return defaultYes, nil
	default:
		return false, nil
	}
}

// readStandalonePassword prompts for username's password on the standalone machine being
// added as name without a private key, for desk machine add to save.
func readStandalonePassword(name, username string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
		return "", &usageError{msg: fmt.Sprintf("%s needs a private key or a password: pass --private-key "+
			"(see `desk keygen`), or run desk machine add in a terminal to type the password for %s",
			name, username)}
	}

	fmt.Fprintf(os.Stderr, "Password for %s on %s: ", username, name)
	pwd, err := term.ReadPassword(int(os.Stdin.Fd())) // #nosec
	fmt.Fprint(os.Stderr, "\r\n")
	if err != nil {
		return "", err
	}
	if len(pwd) == 0 {
		return "", &usageError{msg: "the password can't be empty"}
	}

	return string(pwd), nil
}
