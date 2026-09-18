# Security

ariadne lets a language model call tools on your computer. This page says,
plainly, what protects you and what does not.

## What protects you

- **Approval.** Tools that change things ask first: `write_file` when you pass
  `-approve write_file`, every MCP tool unless you list it in `-trust`, and
  `exec` always. The terminal shows a `[y/N]` prompt; the browser shows a card
  with the exact arguments. No answer is not a yes.
- **The workspace.** `fetch` and `write_file` cannot reach outside the
  workspace folder; the operating system enforces it (`os.Root`), including
  against Windows directory junctions.
- **Your keys.** ariadne's own provider keys are redacted from anything a tool
  returns, and a provider's key is only ever sent to that provider's host.
  `exec` runs with a short list of environment variables, not ariadne's.
- **Untrusted text is marked.** What `fetch`, MCP tools and `exec` return is
  wrapped as data, and the model is told not to follow instructions inside it.

## What does not

- **`exec` is not confined.** It starts in the workspace, but a program can
  read or write anything you can. The approval card is the only control, so
  read it — including the whole of a `python -c` script.
- **Whatever a tool reads is sent to your model provider.** Approving a read
  approves sending what it reads. Redaction covers only ariadne's own keys.
- **Marking text as untrusted asks the model to behave; it does not make it.**
  A document can still talk the model into requesting something. That is why
  the approval gate exists.
- **Trusting a read tool re-opens exfiltration.** With a filesystem server's
  read tool in `-trust`, a document in the workspace can get the model to read
  a secret file next to it and repeat it in its answer. Keep secrets out of
  the workspace.

The attacks behind every line above, with traces, are in
[`docs/injection-postmortem.md`](docs/injection-postmortem.md).

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting ("Report a vulnerability"
on the repository's Security tab) rather than a public issue. Include
`ariadne version`, your OS, and the smallest steps that reproduce it.
