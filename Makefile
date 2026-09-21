# ariadne — development tasks.
#
# `make check` is what to run before a milestone commit. Two of its targets are
# audits that exist because both failures actually happened here:
#
#   audit-tracked    a .go file silently excluded by .gitignore — cmd/ariadne went
#                    two commits without ever being in the repository, because
#                    `git add -A` skips ignored files with no warning
#   audit-livetests  a live test with no build tag, making real API calls on every
#                    `go test ./...` until it exhausted the free tier
#   audit-docnames   a function inserted between a doc comment and the function
#                    it documents, silently reattaching the prose to the wrong
#                    declaration — five times, the fifth found by this target
#
# None of the three is caught by go build, go vet, or a green test run.

ifeq ($(OS),Windows_NT)
    # On Windows, GNU Make defaults to cmd.exe unless sh.exe is found.
    # Locate Git's sh and tools so recipes run under a POSIX shell.
    GIT_SH := $(wildcard C:/PROGRA~1/Git/bin/sh.exe C:/Program\ Files/Git/bin/sh.exe C:/Program\ Files\ \(x86\)/Git/bin/sh.exe)
    ifneq ($(GIT_SH),)
        SHELL := $(firstword $(GIT_SH))
        export PATH := C:/PROGRA~1/Git/usr/bin:$(PATH)
    else
        SHELL := sh.exe
    endif
    .SHELLFLAGS := -c
endif

GO     ?= go
BINARY := ariadne
PKGS   := ./...

.PHONY: all build fmt vet test check live eval workspace audit-tracked audit-livetests audit-docnames audit-skips clean

all: check build

build:
	$(GO) build -o $(BINARY) ./cmd/ariadne

fmt:
	@out=$$(gofmt -l .); [ -z "$$out" ] || { echo 'unformatted:'; echo "$$out"; exit 1; }

vet:
	$(GO) vet $(PKGS)
	$(GO) vet -tags live $(PKGS)

test:
	$(GO) test $(PKGS)

# The pinned eval baseline. Kept here rather than typed at a prompt so a pass
# rate cannot drift because somebody used a different model, and so the history
# in eval/history is comparable with itself.
EVAL_MODEL ?= deepseek/deepseek-v4-flash-0731
EVAL_URL   ?= https://openrouter.ai/api/v1
EVAL_TASKS ?= testdata/tasks.json

eval:
	$(GO) run ./cmd/ariadne eval --base-url $(EVAL_URL) --models $(EVAL_MODEL) --tasks $(EVAL_TASKS) --save

# The daily set: documents, folders, edits and an injection attempt. Real model
# calls, so it is never part of `make check`.
eval-daily:
	$(GO) run ./cmd/ariadne eval --base-url $(EVAL_URL) --models $(EVAL_MODEL) --tasks testdata/daily/tasks.json --repeat 3

# Stage the fetchable fixtures into the sandbox the tools are confined to.
#
# workspace/ is gitignored, because it is scratch space a run writes into. The
# documents are not scratch — they are the fixtures docs/injection-postmortem.md
# is written against, so a fresh clone has to be able to put them back or none
# of that document is reproducible.
workspace:
	@mkdir -p workspace
	@cp testdata/pages/* workspace/
	@echo "workspace: staged $$(ls testdata/pages | wc -l) fixtures"

# Real API calls. Needs a key and spends quota.
live:
	$(GO) test $(PKGS) -tags live -run TestLive -v -count=1

check: fmt vet test audit-tracked audit-livetests audit-docnames audit-skips
	@echo 'check: ok'

# Every .go file must be known to git.
audit-tracked:
	@missing=0; \
	for f in $$(find . -name '*.go' -not -path './.git/*' | sed 's|^\./||'); do \
	  git ls-files --error-unmatch "$$f" >/dev/null 2>&1 || { echo "NOT IN GIT: $$f"; missing=1; }; \
	done; \
	[ $$missing -eq 0 ] || { echo 'audit-tracked: FAILED'; exit 1; }

# No TestLive* may appear in the untagged build. Compiling under a tag is not
# the same as being excluded without it, which is why vet cannot catch this.
audit-livetests:
	@leaked=0; \
	for p in $$($(GO) list $(PKGS)); do \
	  if $(GO) test "$$p" -list 'TestLive.*' 2>/dev/null | grep -q '^TestLive'; then \
	    echo "LIVE TEST IN OFFLINE SUITE: $$p"; leaked=1; \
	  fi; \
	done; \
	[ $$leaked -eq 0 ] || { echo 'audit-livetests: FAILED'; exit 1; }

# A doc comment must name the declaration it sits on. The rule and the reason
# are in the script; it is awk rather than inline here because the Makefile
# escaping made it unreadable.
audit-docnames:
	@bad=0; 	for f in $$(find . -name '*.go' -not -path './.git/*'); do 	  awk -f scripts/audit-docnames.awk "$$f" "$$f" || bad=1; 	done; 	[ $$bad -eq 0 ] || { echo 'audit-docnames: FAILED'; exit 1; }

# Security-relevant tests (Sandbox|Injection|Gate|Trust|Redact) must not skip
# without a recorded entry and justification in scripts/audit-skips.go.
audit-skips:
	@$(GO) run ./scripts/audit-skips.go

clean:
	rm -f $(BINARY) $(BINARY).exe
