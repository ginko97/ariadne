# Contributing

Thanks for looking. Issues and pull requests are welcome.

## Build and check

```bash
go build ./cmd/ariadne
make check        # gofmt, vet in both build modes, tests, and three audits
go test -race ./...
```

`make check` must pass. On Windows it runs under Git's `sh`, which the Makefile
finds for you.

## Rules the code already follows

- **No live API calls in tests.** Tests run against a fake provider or a
  recorded HTTP transport. Tests that call a real API carry the `live` build
  tag and run only with `-tags live`; `make check` fails if one leaks.
- **A fix comes with a test that fails without it.** Before sending a fix,
  undo it and watch the test fail, then restore it. A test that passes both
  ways is not testing the fix.
- **Standard library first.** A new dependency needs a reason in the pull
  request.
- **Say what is not done.** If a change has a limit, write it in the comment
  or the commit message rather than leaving it to be discovered.

## Reporting a bug

Include `ariadne version`, your OS, the provider and model, and — if it is
about a conversation — the run id. `ariadne traces -run <id>` shows what
happened; please check it for anything private before pasting it.

Security problems: see [SECURITY.md](SECURITY.md).
