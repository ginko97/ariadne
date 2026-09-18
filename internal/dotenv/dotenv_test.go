package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadParsesExportAndQuotes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/ginko97/ariadne\n\ngo 1.26.4\n"), 0o600); err != nil {
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

// Outside an ariadne checkout, a .env in the working directory is somebody
// else's. An installed binary run in a project folder used to load that
// project's .env into its own environment; config.env in the data directory
// replaced that path.
func TestLoadIgnoresADotenvOutsideTheCheckout(t *testing.T) {
	for name, goMod := range map[string]string{
		"no go.mod":      "",
		"another module": "module example.com/someone/else\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if goMod != "" {
				if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TEST_DOTENV_FOREIGN=leaked\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			defer os.Unsetenv("TEST_DOTENV_FOREIGN")

			if err := Load(); err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := os.Getenv("TEST_DOTENV_FOREIGN"); got != "" {
				t.Errorf("a .env outside an ariadne checkout was loaded: %q", got)
			}
		})
	}
}

// LoadFile is how config.env in the data directory is read: same parsing, and
// the process environment still wins.
func TestLoadFileKeepsExistingVariables(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(p, []byte("TEST_DOTENV_CFG=from-file\nTEST_DOTENV_SET=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_DOTENV_SET", "from-env")
	defer os.Unsetenv("TEST_DOTENV_CFG")

	if err := LoadFile(p); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("TEST_DOTENV_CFG"); got != "from-file" {
		t.Errorf("TEST_DOTENV_CFG = %q, want from-file", got)
	}
	if got := os.Getenv("TEST_DOTENV_SET"); got != "from-env" {
		t.Errorf("the file overrode the environment: %q", got)
	}
	if err := LoadFile(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Errorf("a missing file is an error: %v", err)
	}
}

func TestLoadPreservesMismatchedAndTrailingQuotes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/ginko97/ariadne\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := "TEST_DOTENV_P1=pass\"\nTEST_DOTENV_P2=\"pass'\nTEST_DOTENV_P3=\"\"double\"\"\n"
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
		os.Unsetenv("TEST_DOTENV_P1")
		os.Unsetenv("TEST_DOTENV_P2")
		os.Unsetenv("TEST_DOTENV_P3")
	}()

	if got := os.Getenv("TEST_DOTENV_P1"); got != "pass\"" {
		t.Errorf("TEST_DOTENV_P1 = %q, want pass\"", got)
	}
	if got := os.Getenv("TEST_DOTENV_P2"); got != "\"pass'" {
		t.Errorf("TEST_DOTENV_P2 = %q, want \"pass'", got)
	}
	if got := os.Getenv("TEST_DOTENV_P3"); got != "\"double\"" {
		t.Errorf("TEST_DOTENV_P3 = %q, want \"double\"", got)
	}
}
