package main

import (
	"strings"
	"testing"
)

// TestAuditSkipsAllowedOnUnixAndWindows asserts that all registered platform skips
// are accepted without violation.
func TestAuditSkipsAllowedOnUnixAndWindows(t *testing.T) {
	input := `
=== RUN   TestSandboxRejectsSymlinkEscapes
--- SKIP: TestSandboxRejectsSymlinkEscapes (0.00s)
=== RUN   TestSandboxWithSymlinkedRoot
--- SKIP: TestSandboxWithSymlinkedRoot (0.00s)
=== RUN   TestSandboxWindowsPathsAreNamesOnUnix
--- SKIP: TestSandboxWindowsPathsAreNamesOnUnix (0.00s)
=== RUN   TestSandboxWindowsDriveEscape
--- SKIP: TestSandboxWindowsDriveEscape (0.00s)
=== RUN   TestSandboxRejectsJunctionEscape
--- SKIP: TestSandboxRejectsJunctionEscape (0.00s)
=== RUN   TestSandboxWithJunctionedRoot
--- SKIP: TestSandboxWithJunctionedRoot (0.00s)
`
	got := auditSkips(strings.NewReader(input))
	if len(got) != 0 {
		t.Fatalf("expected 0 violations for allowed skips, got %d: %v", len(got), got)
	}
}

// TestAuditSkipsFailsOnUnregisteredSecuritySkip asserts that any security-relevant
// test that skips without registration is detected as a violation.
func TestAuditSkipsFailsOnUnregisteredSecuritySkip(t *testing.T) {
	input := `
=== RUN   TestSandboxBypassUnregistered
--- SKIP: TestSandboxBypassUnregistered (0.00s)
=== RUN   TestApprovalGateBypass
--- SKIP: TestApprovalGateBypass (0.00s)
`
	got := auditSkips(strings.NewReader(input))
	if len(got) != 2 {
		t.Fatalf("expected 2 violations, got %d: %v", len(got), got)
	}
	if got[0] != "TestSandboxBypassUnregistered" || got[1] != "TestApprovalGateBypass" {
		t.Errorf("unexpected violation list: %v", got)
	}
}

// TestAuditSkipsIgnoresNonSecurityTests asserts that non-security test skips
// (e.g. optional tooling or fixtures) are ignored.
func TestAuditSkipsIgnoresNonSecurityTests(t *testing.T) {
	input := `
=== RUN   TestFetchReadsARealPDF
--- SKIP: TestFetchReadsARealPDF (0.00s)
=== RUN   TestSomethingOrdinary
--- SKIP: TestSomethingOrdinary (0.00s)
`
	got := auditSkips(strings.NewReader(input))
	if len(got) != 0 {
		t.Fatalf("expected 0 violations for non-security skips, got %d: %v", len(got), got)
	}
}
