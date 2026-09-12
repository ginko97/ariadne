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
#
# Neither is caught by go build, go vet, or a green test run.

GO     ?= go
BINARY := ariadne
PKGS   := ./...

.PHONY: all build fmt vet test check live audit-tracked audit-livetests clean

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

# Real API calls. Needs a key and spends quota.
live:
	$(GO) test $(PKGS) -tags live -run TestLive -v -count=1

check: fmt vet test audit-tracked audit-livetests
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

clean:
	rm -f $(BINARY) $(BINARY).exe
