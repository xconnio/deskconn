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

// readCredentials returns the username, from --username or else a prompt, and the
// password: prompted for on a terminal, otherwise the first line of stdin, so it never
// appears on the command line.
func readCredentials(flagUsername string) (username, password string, err error) {
	interactive := term.IsTerminal(int(os.Stdin.Fd())) // #nosec

	username = flagUsername
	if username == "" {
		if !interactive {
			return "", "", fmt.Errorf("--username is required when the password comes from stdin")
		}
		fmt.Fprint(os.Stderr, "Username: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return "", "", err
		}
		username = strings.TrimSpace(line)
		if username == "" {
			return "", "", fmt.Errorf("username cannot be empty")
		}
	}

	if interactive {
		password, err = readPassword()
		if err != nil {
			return "", "", err
		}
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", "", fmt.Errorf("reading password from stdin: %w", err)
		}
		password = strings.TrimRight(line, "\r\n")
	}
	if password == "" {
		return "", "", fmt.Errorf("password cannot be empty")
	}

	return username, password, nil
}

func readOTP() (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
		return "", fmt.Errorf("otp required: rerun interactively to enter the one-time password emailed to you")
	}

	fmt.Fprint(os.Stderr, "OTP: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	otp := strings.TrimSpace(line)
	if otp == "" {
		return "", fmt.Errorf("otp cannot be empty")
	}

	return otp, nil
}

// readStandalonePassword prompts for authID's password on the standalone machine, for a
// profile saved without a private key, when a command first connects.
func readStandalonePassword(authID string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
		return "", fmt.Errorf("the password for %s can only be typed in a terminal; to connect without one, "+
			"save the profile with --private-key (see `desk keygen`)", authID)
	}

	fmt.Fprintf(os.Stderr, "Password for %s: ", authID)
	pwd, err := term.ReadPassword(int(os.Stdin.Fd())) // #nosec
	fmt.Fprint(os.Stderr, "\r\n")
	if err != nil {
		return "", err
	}

	return string(pwd), nil
}

func readPassword() (string, error) {
	fmt.Fprint(os.Stderr, "Password: ")
	pwd, err := term.ReadPassword(int(os.Stdin.Fd())) // #nosec
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}

	return string(pwd), nil
}
