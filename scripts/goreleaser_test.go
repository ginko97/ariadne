package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The release notes are the annotated tag's message, passed with
// --release-notes. GoReleaser reads that file in the changelog pipe's Run,
// which never executes when changelog.disable is true — so the setting
// discards the notes without an error, and the only symptom is a blank
// release page after the tag is public.
//
// It happened: 1f10e60 added --release-notes and kept disable: true, and
// v0.5.0 through v0.6.1 all shipped empty. Nothing in make check, CI or the
// release job could see it, which is why this reads the config directly.
func TestChangelogIsNotDisabledSoReleaseNotesSurvive(t *testing.T) {
	cfg, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Any uncommented `disable:` directly under a top-level `changelog:` key.
	block := regexp.MustCompile(`(?m)^changelog:\s*\n((?:[ \t]+.*\n?|\s*\n)*)`)
	if m := block.FindSubmatch(cfg); m != nil {
		for _, line := range strings.Split(string(m[1]), "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "#") {
				continue
			}
			if strings.HasPrefix(l, "disable:") && !strings.Contains(l, "false") {
				t.Fatalf("changelog.disable is set (%q): GoReleaser will drop the "+
					"--release-notes file and every release page will be blank", l)
			}
		}
	}

	wf, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wf), "--release-notes=") {
		t.Error("the release workflow no longer passes --release-notes; the tag " +
			"message will not reach the release page")
	}
}
