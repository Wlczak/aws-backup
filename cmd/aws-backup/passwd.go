package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Wlczak/aws-backup/internal/config"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

func runPasswd(path string) {
	created, err := ensureProfileLayout(path)
	if err != nil {
		fatalf("prepare config %s: %v", path, err)
	}
	if created {
		fmt.Fprintf(os.Stderr, "created default config layout at %s\n", path)
	}

	pw1, err := promptPassword("New password: ")
	if err != nil {
		fatalf("read password: %v", err)
	}
	pw2, err := promptPassword("Confirm password: ")
	if err != nil {
		fatalf("read password confirmation: %v", err)
	}
	if pw1 != pw2 {
		fatalf("passwords do not match")
	}
	if err := setCentralPasswordHash(path, pw1); err != nil {
		fatalf("%v", err)
	}
	fmt.Fprintf(os.Stderr, "updated password in %s\n", path)
}

func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(pw), nil
}

func setCentralPasswordHash(path, password string) error {
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("password cannot be empty")
	}
	central, err := config.LoadCentral(path)
	if err != nil {
		return fmt.Errorf("load central config %s: %w", path, err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	central.Auth.PasswordHash = string(hash)
	if err := config.SaveCentral(path, central); err != nil {
		return fmt.Errorf("save central config: %w", err)
	}
	return nil
}
