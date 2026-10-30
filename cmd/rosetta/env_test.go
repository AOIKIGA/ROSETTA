package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialFileParsing(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"# comment\nOTHER_KEY=ignored\nDEEPSEEK_API_KEY = local-test-key\n", "local-test-key"},
		{"export DEEPSEEK_API_KEY='quoted-test-key' # comment\r\n", "quoted-test-key"},
		{"DEEPSEEK_API_KEY=\"quoted-test-key\"", "quoted-test-key"},
		{"DEEPSEEK_API_KEY=first\nDEEPSEEK_API_KEY=second # comment", "second"},
		{"DEEPSEEK_API_KEY='$(do-not-execute) ${NO_EXPANSION}'", "$(do-not-execute) ${NO_EXPANSION}"},
	} {
		got, err := parseDeepSeekKey(tc.text)
		if err != nil || got != tc.want {
			t.Fatal("credential parse mismatch")
		}
	}
	for _, text := range []string{"DEEPSEEK_API_KEY", "DEEPSEEK_API_KEY='private-unclosed", "DEEPSEEK_API_KEY=\"private\" trailing", "DEEPSEEK_API_KEY=private\x00"} {
		if _, err := parseDeepSeekKey(text); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("malformed assignment accepted or value exposed")
		}
	}
}

func TestCredentialEnvironmentPrecedenceAndOptionalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("DEEPSEEK_API_KEY=file-test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "environment-test-key")
	if got, err := deepSeekKey(path); err != nil || got != "environment-test-key" {
		t.Fatal("environment did not take precedence")
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if got, err := deepSeekKey(path); err != nil || got != "" {
		t.Fatal("explicitly empty environment did not take precedence")
	}
	if err := os.Unsetenv("DEEPSEEK_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if got, err := deepSeekKey(path); err != nil || got != "file-test-key" {
		t.Fatal("file credential not loaded")
	}
	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing.env")} {
		if got, err := deepSeekKey(path); err != nil || got != "" {
			t.Fatal("missing/disabled file must be optional")
		}
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", (64<<10)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := deepSeekKey(path); err == nil {
		t.Fatal("unbounded credential file read")
	}
}
