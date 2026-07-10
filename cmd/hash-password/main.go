package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"golang.org/x/term"
)

const maximumPasswordBytes = 1024

func main() {
	password, err := readPassword(os.Stdin, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read password:", err)
		os.Exit(1)
	}
	defer clear(password)

	hash, err := auth.HashArgon2ID(string(password), rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot hash password:", err)
		os.Exit(1)
	}
	fmt.Println(hash)
}

func readPassword(input *os.File, output io.Writer) ([]byte, error) {
	fd := int(input.Fd())
	if !term.IsTerminal(fd) {
		value, err := io.ReadAll(io.LimitReader(input, maximumPasswordBytes+3))
		if err != nil {
			return nil, errors.New("read standard input")
		}
		return normalizePipedPassword(value)
	}

	fmt.Fprint(output, "New admin password: ")
	password, err := term.ReadPassword(fd)
	fmt.Fprintln(output)
	if err != nil {
		return nil, errors.New("read hidden password")
	}
	if len(password) > maximumPasswordBytes {
		clear(password)
		return nil, errors.New("password is too long")
	}
	fmt.Fprint(output, "Confirm password: ")
	confirmation, err := term.ReadPassword(fd)
	fmt.Fprintln(output)
	if err != nil {
		clear(password)
		return nil, errors.New("read hidden confirmation")
	}
	defer clear(confirmation)
	if !bytes.Equal(password, confirmation) {
		clear(password)
		return nil, errors.New("passwords do not match")
	}
	return password, nil
}

func normalizePipedPassword(value []byte) ([]byte, error) {
	value = bytes.TrimSuffix(value, []byte("\n"))
	value = bytes.TrimSuffix(value, []byte("\r"))
	if len(value) > maximumPasswordBytes {
		clear(value)
		return nil, errors.New("password is too long")
	}
	if bytes.ContainsAny(value, "\r\n") {
		clear(value)
		return nil, errors.New("password must be provided as one line")
	}
	return value, nil
}
