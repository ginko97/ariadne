package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadParsesExportAndQuotes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0o600); err != nil {
		t.Fatal(err)
	}

	content := "# Comment line\nTEST_DOTENV_A=simple\nexport TEST_DOTENV_B=\"quoted value\"\nexport TEST_DOTENV_C='single quoted'\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	defer func() {
		os.Unsetenv("TEST_DOTENV_A")
		os.Unsetenv("TEST_DOTENV_B")
		os.Unsetenv("TEST_DOTENV_C")
	}()

	if got := os.Getenv("TEST_DOTENV_A"); got != "simple" {
		t.Errorf("TEST_DOTENV_A = %q, want simple", got)
	}
	if got := os.Getenv("TEST_DOTENV_B"); got != "quoted value" {
		t.Errorf("TEST_DOTENV_B = %q, want 'quoted value'", got)
	}
	if got := os.Getenv("TEST_DOTENV_C"); got != "single quoted" {
		t.Errorf("TEST_DOTENV_C = %q, want 'single quoted'", got)
	}
}

func TestLoadWithoutGoMod(t *testing.T) {
	dir := t.TempDir()
	content := "TEST_DOTENV_STANDALONE=works\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	defer os.Unsetenv("TEST_DOTENV_STANDALONE")

	if got := os.Getenv("TEST_DOTENV_STANDALONE"); got != "works" {
		t.Errorf("TEST_DOTENV_STANDALONE = %q, want works", got)
	}
}
