# Security

ariadne lets a language model call tools on your computer. This page says,
plainly, what protects you and what does not.

## What protects you

- **Approval.** Tools that change things or reach out ask first: `write_file`,
  `edit_file` and `web_fetch` unless you list them in `-trust`, every MCP
  tool unless you trust it, and `exec` always — nothing can trust `exec`
  away. The terminal shows a `[y/N]` prompt; the browser shows a card. Both
  say what the call would do — the file and the lines an edit changes, what
  a write replaces, the URL in full, the program and its arguments — with
  the exact arguments still there to open. No answer is not a yes, and with
  no terminal to ask, a gated call is denied.
- **Allowing a site for the rest of a turn — what it does and does not do.**
  In the browser, several calls the model makes at once to one tool arrive as
  one card, every call listed in full. A `web_fetch` card can also allow a
  destination until the turn ends, so the next fetch to it does not ask
  again. The destination is the *origin* — scheme, host and port — so
  allowing `https://github.com` does not cover `http://github.com` or
  `api.github.com`. A grant ends with the turn: it is never saved, never
  carried to your next message, and never widened when a conversation is
  resumed. Each one is recorded in the trace, with the calls it let through.
  Only `web_fetch` can be allowed this way; `write_file` and `edit_file`
  ask for every change, and `exec` for every program.

  **What a grant does not protect:** anything the model puts in a URL to an
  allowed destination reaches it, without asking. That is the decision you
  made when you allowed it, and why nothing is allowed by default. Any page
  on an allowed site can be read without asking, not only the one you saw.
  The terminal has no grants; it asks for every call.
- **A redirect to another site is not followed.** Approving a fetch, or
  allowing a site, approves that site. When a page redirects somewhere else,
  `web_fetch` stops and tells the model where it was sent; reading that page
  is a new fetch, which asks unless that site is allowed too. Two redirects
  are still followed without asking: within the same site (same scheme, host
  and port), and `http` upgraded to `https` on the same host. Before v0.6.7
  every redirect was followed, so an *open redirect* on an allowed site could
  pass a request on to a site you never saw.
- **What is not protected: the download itself.** The release binaries are
  **not code-signed**, so Windows SmartScreen warns about them and macOS
  Gatekeeper may refuse them. The only integrity check this project offers is
  `checksums.txt` on the release page — verify the archive against it before
  running anything. A signed binary would prove who built it; a checksum only
  proves the file did not change in transit from a page that could itself be
  wrong. Judge the source first.
- **Deleting is deleting.** Removing a conversation in the browser removes
  its folder: the checkpoint and the trace, which is every byte that
  conversation saw. Nothing is kept for later and nothing comes back.
- **The workspace.** `fetch`, `list_files`, `write_file` and `edit_file` cannot reach outside the
  workspace folder; the operating system enforces it (`os.Root`), including
  against Windows directory junctions.
- **Your keys.** ariadne's own provider keys are redacted from anything a tool
  returns, and a provider's key is only ever sent to that provider's host.
  `exec` runs with a short list of environment variables, not ariadne's.
- **Setting up in the browser.** The key you type goes only to ariadne on
  your machine: the page is served on `127.0.0.1` only, and the setup request
  must carry the page's secret token and a local `Host` and `Origin`, so
  another website cannot change your provider or key. The key is checked with
  one request to the provider you chose, stored in `config.env`, and never
  sent back to the page — not even masked, not even in an error message.
- **Your network.** `web_fetch` refuses loopback, private, link-local and
  other non-public addresses, checked on the address actually connected to — so
  a redirect or a name that resolves to your router is refused too, even when
  you approved the fetch. It sends no cookies and no credentials.
- **Answers cannot load anything by themselves.** The browser formats the
  model's Markdown by building page elements, never by inserting HTML, so
  markup quoted from a document is shown, not run. Images are never loaded:
  a Markdown image would fetch its URL the moment it is shown, which is a
  way to send what the model read to another server without a click. Links
  are `http(s)` only, show their address, and send no referrer.
- **Untrusted text is marked.** What `fetch`, `web_fetch`, MCP tools and `exec`
  return is wrapped as data, and the model is told not to follow instructions
  inside it.

## What does not

- **`exec` is not confined.** It starts in the workspace, but a program can
  read or write anything you can. The approval card is the only control, so
  read it — including the whole of a `python -c` script.
- **A URL can carry data out.** Approving `web_fetch` approves sending the
  whole URL to that site, and a URL can hold anything the model has read
  (`https://example.com/?q=...`). Read the URL on the card before approving;
  `-trust web_fetch` removes that check.
- **Reading a PDF runs another program.** When Poppler's `pdftotext` is
  installed, `fetch` runs it on a copy of the PDF, without asking, as it
  reads any file. A PDF crafted against a bug in `pdftotext` would run with
  your permissions. Keep Poppler updated, or uninstall it and PDFs are
  refused. Office and LibreOffice files are read by ariadne itself.
- **Whatever a tool reads is sent to your model provider.** Approving a read
  approves sending what it reads. Redaction covers only ariadne's own keys.
  Reading files in the workspace does not ask by default; start with
  `-approve fetch,list_files` (in the terminal or with `ariadne ui`) to be
  asked before every read, at the cost of a card on almost every step.
- **Marking text as untrusted asks the model to behave; it does not make it.**
  A document can still talk the model into requesting something. That is why
  the approval gate exists.
- **Trusting a read tool re-opens exfiltration.** With a filesystem server's
  read tool in `-trust`, a document in the workspace can get the model to read
  a secret file next to it and repeat it in its answer. Keep secrets out of
  the workspace.
- **A brief is your own instruction.** A brief (`-task`, **Brief…**,
  `/brief`) is sent as your message, not as untrusted text, so nothing marks
  it and the model treats every line as yours. Use briefs you wrote. A
  downloaded `.md` used as a brief has the authority a web page is denied.
  The browser shows the whole brief before it starts and refuses to start if
  the file changed since; the terminal prints it and starts at once.
  Approval still applies to what the brief leads to. A conversation started
  from a brief may take 25 steps a turn instead of 10, so it can make more
  model calls, and spend more, before it stops on its own.
- **"Cited but never opened" is not fact-checking.** Under an answer, ariadne
  lists the URLs the answer, or a file written in that turn, cites that no
  fetch in the conversation opened. It checks URLs only: a source named by
  its title is not checked, and a page that was opened can still be
  misquoted. A page opened by an MCP tool counts only if the tool takes a
  `url` argument.

The attacks behind every line above, with traces, are in
[`docs/injection-postmortem.md`](docs/injection-postmortem.md).

## Reporting a vulnerability

Please report it privately rather than in a public issue: use GitHub's
private vulnerability reporting ("Report a vulnerability" on the repository's
Security tab). If that button is not there, open an issue that says only that
you have a security report and asks for a private contact — no details in it.

Include `ariadne version`, your OS, and the smallest steps that reproduce it.
