package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
)

// allowedSkips records security-relevant tests that are permitted to skip on
// specific platforms, along with the reason. Any security-relevant test matching
// Sandbox|Injection|Gate|Trust|Redact that skips without being listed here fails
// make check.
var allowedSkips = map[string]string{
	// Windows unprivileged accounts cannot create symlinks without Developer Mode.
	// Junction-based equivalents (TestSandboxRejectsJunctionEscape, TestSandboxWithJunctionedRoot)
	// cover this ground unprivileged.
	"TestSandboxRejectsSymlinkEscapes": "symlinks not supported on this platform",
	"TestSandboxWithSymlinkedRoot":     "symlinks not supported on this platform",

	// Windows drive letters and UNC paths are absolute on Windows and ordinary names on Unix.
	// TestSandboxWindowsDriveEscape tests rejection on Windows; on Unix it skips because
	// TestSandboxWindowsPathsAreNamesOnUnix asserts the Unix behaviour instead.
	"TestSandboxWindowsPathsAreNamesOnUnix": "covered by TestSandboxWindowsDriveEscape on Windows",
	"TestSandboxWindowsDriveEscape":         "drive letters and UNC paths are absolute only on Windows",
}

// securityPattern matches tests whose names indicate security-critical invariants.
var securityPattern = regexp.MustCompile(`(Sandbox|Injection|Gate|Trust|Redact)`)

// skipRe extracts the test name from go test verbose output.
var skipRe = regexp.MustCompile(`^--- SKIP:\s+(\w+)`)

// auditSkips scans test output for unrecorded skips of security-relevant tests.
func auditSkips(r io.Reader) []string {
	var violations []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		m := skipRe.FindStringSubmatch(line)
		if len(m) > 1 {
			testName := m[1]
			if securityPattern.MatchString(testName) {
				if _, ok := allowedSkips[testName]; !ok {
					violations = append(violations, testName)
				}
			}
		}
	}
	return violations
}

func main() {
	var r io.Reader
	var cmd *exec.Cmd

	if len(os.Args) > 1 && os.Args[1] == "-stdin" {
		r = os.Stdin
	} else {
		cmd = exec.Command("go", "test", "-v", "./...")
		cmd.Stderr = os.Stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit-skips: failed to pipe stdout: %v\n", err)
			os.Exit(1)
		}
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "audit-skips: failed to start go test: %v\n", err)
			os.Exit(1)
		}
		r = out
	}

	violations := auditSkips(r)

	if cmd != nil {
		if err := cmd.Wait(); err != nil {
			fmt.Fprintf(os.Stderr, "audit-skips: go test exited with error: %v\n", err)
			os.Exit(1)
		}
	}

	if len(violations) > 0 {
		fmt.Fprintf(os.Stderr, "audit-skips: FAILED — security-relevant tests skipped without recorded justification:\n")
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "  %s\n", v)
		}
		os.Exit(1)
	}
}
