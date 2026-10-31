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

// readPassword reads a password from stdin. On a terminal it prompts twice
// without echo; otherwise it reads the first line (so it can be piped in).
func readPassword(stdin *os.File, prompt io.Writer) (string, error) {
	fd := int(stdin.Fd()) //nolint:gosec // G115: file descriptors fit in int
	if term.IsTerminal(fd) {
		fmt.Fprint(prompt, "Password: ")
		pw, err := term.ReadPassword(fd)
		fmt.Fprintln(prompt)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		fmt.Fprint(prompt, "Repeat password: ")
		again, err := term.ReadPassword(fd)
		fmt.Fprintln(prompt)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if string(pw) != string(again) {
			return "", errors.New("passwords do not match")
		}
		return string(pw), nil
	}
	return readLine(stdin)
}

// readLine returns the first line of r without the line ending.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		return "", errors.New("no password on stdin")
	}
	return line, nil
}
