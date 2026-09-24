# Changelog

What changed for someone using ariadne. The tag messages (`git show v0.3.0`)
carry the longer story for each release.

## v0.6.8 — 2026-09-24 — cited but never opened, and a week of daily use

- **Cited but never opened.** After an answer, ariadne checks every URL the
  answer, or a file written in that turn, cites against the pages the
  conversation actually opened. Any it never opened — a guessed link, or a
  page that failed every time it was fetched — are listed under the answer
  in the browser, and after it on stderr in the terminal. It checks links,
  not what they say, and a source named only by its title is not checked.
  Run over the week's real conversations, it found a cited page in the QRIS
  report that timed out all three times it was fetched.
- **Brief… lists the briefs.** The panel shows the `.md` files in the
  conversation's folder, subfolders included, newest first; click one to
  read it. Typing a path still works. Empty files and files over 2 MB are
  left out, since neither can be started.
- **`-task` takes only a `.md` file**, in `ariadne run` and `ariadne chat`,
  as **Brief…** already did. A brief in a `.txt` file, which used to work,
  is now refused: rename it. A folder, a file over 2 MB, or one with only
  blank lines is refused before anything is sent, with a message saying
  which.
- **An empty brief is refused in the browser too**, instead of reaching the
  provider as an empty message and failing there.
- **Reopening a conversation started from a brief shows the brief** as the
  brief card it started with, not as a plain message.
- **A brief gets 25 steps a turn**, up from 10, unless `-max-steps` is
  given. A research brief used exactly the 10 a turn allowed. Every gated
  call still asks.
- **A released binary keeps its data in one place.** Started from a
  terminal inside an ariadne source checkout, it used the checkout's
  `runs/` and `.env` instead of `%AppData%\ariadne`, and the day's
  conversations were missing from the usual folder. Now only a development
  build uses a checkout. `ariadne ui` prints the folder in use, and **⚙**
  shows it. If you ran a release from inside a checkout, those
  conversations are still in its `runs/`.
- **No Browse… where it cannot work.** The button shows only when a folder
  dialog is installed; in WSL without `zenity` you type the path.
- **The message box grows** with what you type or paste, up to about ten
  lines, instead of showing only the first line.

## v0.6.7 — 2026-09-23 — a redirect to another site asks again

- **A redirect to another site is no longer followed.** Approving a fetch,
  or allowing a site until the turn ends, approves that site. Until now a
  page that redirected somewhere else was followed without asking, so an
  open redirect on an allowed site could pass a request on to a site you
  never saw. Now `web_fetch` stops and tells the model where it was sent;
  reading that page is a new fetch, which asks unless that site is allowed
  too. Redirects within the same site, and `http` upgraded to `https` on
  the same host, are still followed. This applies even with
  `-trust web_fetch`.
- **SECURITY.md names the read gate that already existed.** Start with
  `-approve fetch,list_files` to be asked before every file read.
- **The README shows kill-and-resume** near the top.

## v0.6.6 — 2026-09-23 — try again, and see what it can do

- **Try again, for an answer that never arrived.** When the connection drops,
  the provider fails, or you press Stop mid-answer, the page now offers
  **Try again**. It asks the model the same question again, adds nothing to
  the conversation, and runs no tool a second time. You no longer have to
  type "continue". Reopening such a conversation shows the same button.
- **The page says what a conversation can do.** The top bar shows how many
  tools are on and how many ask first, says when ariadne can run programs or
  has MCP tools, and names in red anything that normally asks but was
  exempted with `-trust`. **⚙** lists every tool and shows the version.
  These are still set only when `ariadne ui` starts. The page shows them;
  it cannot change them.
- **A cut-off answer is no longer saved as if it were complete.** If the
  connection closed cleanly partway through an answer, the fragment that had
  arrived ("IHSG closed at 7,1") was kept as the whole answer. A stream that
  ends without the provider saying it finished is now an error, and nothing
  from it is saved.

## v0.6.5 — 2026-09-23 — a brief is shown before it runs

- **A brief is shown before it runs.** In the browser, **Brief…** and
  `/brief <file.md>` now open the brief in full, and nothing is sent to the
  model until **Start this brief**. If the file changed after it was shown,
  starting is refused, because what you agreed to has to be what runs. In
  v0.6.4 the brief started at once and appeared as the model began reading it.
- **SECURITY.md says what a brief is:** your own instruction, not untrusted
  text. Use briefs you wrote.
- **A paused card closes.** When a card in a brief conversation stops
  waiting, its buttons are disabled and it points at **Resume**. Before this
  they stayed clickable and returned an error.
- **`chat -task` prints the conversation id**, like any first message does.
- **A new conversation appears in the sidebar as soon as it asks you
  something.** It used to be missing until its first turn ended, so a first
  turn waiting on a card had no row and no **Needs you**.
- **The README is shorter, with a screenshot.** The design and the evidence
  behind it (crash recovery, measurement, the injection findings) moved to
  [ARCHITECTURE.md](ARCHITECTURE.md), unchanged.
- **A refused message no longer freezes the page.** When the server turned a
  message away (the conversation already busy in another tab, no provider
  set up, ariadne unreachable), the page stayed on "Working…" with Send
  hidden until you reloaded it.
- **MCP SDK 1.8.0** (from 1.7.0). Same protocol (2026-07-28). Per its
  release notes, a cancelled tool call is now given up at once instead of
  waiting up to five seconds for the cancel notice to be delivered, and JSON
  nested past 1000 levels is refused.

## v0.6.4 — 2026-09-23 — research briefs & jobs that wait for you

- **Research briefs (`-task <file.md>` and `/brief <file.md>`).** Seed a
  conversation from a markdown task brief in your workspace rather than
  typing long prompts at the composer. Supported via `ariadne run -task brief.md`,
  `ariadne chat -task brief.md`, the new **Brief…** button in the browser UI,
  and the `/brief <file.md>` chat command. The brief content is displayed in
  full before running so you always know what instructions are being executed.
- **"Needs you" indicator in the sidebar.** Conversations with active approval
  cards or pending tool calls from an interrupted turn are highlighted with a
  prominent "Needs you" badge in the conversation sidebar, so you can easily
  spot which conversations require operator review.
- **Cards that wait instead of timing out to a denial.** For conversations
  started from a brief, unanswered approval cards release the conversation's
  in-flight claim after the 5-minute timeout and pause gracefully instead of
  timing out into a permanent denial. The tool calls remain safely pending on
  disk, the conversation is marked "Needs you", and resuming the turn allows
  you to review and approve the actions at your convenience.

## v0.6.3 — 2026-09-22 — one card per decision (approval fatigue)

- **One card per decision, not one per call.** A research request that read
  twenty web pages used to raise twenty approval cards, and nobody reads the
  twentieth — which quietly turns "asks before it acts" into "clicks yes
  before it acts". Now, in the browser, several calls the model makes at once
  to one tool arrive as one card listing every call, with a box to leave any
  of them out. A web fetch card can also **allow that site until the turn
  ends**: later fetches to it go ahead without asking, and a fetch to any
  other site still asks. A grant is never saved, never carried to your next
  message, and is recorded in the trace with the calls it let through. Only
  web fetches can be allowed this way; writes, edits and programs still ask
  every time. See SECURITY.md for what a grant does not protect.
- **Windows first-launch scan delay documented.** Added documentation for the
  intermittent SmartScreen / antivirus file lock on unsigned binaries during
  first launch on Windows.

## v0.6.2 — 2026-09-22 — terminal stdin ownership & release notes

- **An answer typed after a terminal prompt times out is no longer lost.**
  Since v0.5.4, a prompt that timed out left a reader behind on the
  terminal, and the next line you typed went to it and was thrown away —
  so a "y" meant for the next approval was dropped, and that prompt waited
  for whatever you typed after it, possibly your next message. The
  terminal now has one reader for the whole session; a prompt that stops
  waiting takes nothing with it.
- **Release pages on GitHub show the release notes.** Every release from
  v0.5.0 to v0.6.1 published an empty page, because a setting in the
  release configuration made GoReleaser discard them.

## v0.6.1 — 2026-09-22 — correctness and concurrency hardening

- **Disjoint grant rejection on resume (B1).** `checkResumeGrants` now verifies that
  when a resuming command specifies `--allow`, it shares at least one common tool
  with the checkpoint's existing allow-list. If disjoint, resume fails with an
  explicit, actionable error instead of silently continuing with the checkpoint's grant.
- **Post-compaction token synthesis (B3).** Replaced the `s.InputTokens = 0` reset
  with a conservative token count synthesized proportionally from retained characters.
  Keeps the token count below the context budget to prevent double-compaction cascades
  on interrupted resumes while eliminating the post-compaction blind estimation window.
- **Thread-safe memory store (B5).** Introduced an in-process path-keyed mutex
  serializing `memory.Store.Append` and `Delete`, closing a TOCTOU race where an
  asynchronous background turn's `remember` append could be overwritten by a concurrent
  deletion from the web UI.
- **Strict cell reference validation (B4).** `columnIndex` now validates that cell
  references have one or more ASCII digits after column letters, rejecting malformed
  inputs like `A1B` or `A` and preventing silent row alignment drift in XLSX spreadsheets.
- **GitHub Actions runner modernization.** Upgraded workflow actions to Node 24-native
  pinned releases (`actions/checkout` v7.0.1, `actions/setup-go` v7.0.0 with `cache: false`,
  `goreleaser-action` v7.2.3, and `attest-build-provenance` v4.2.2), eliminating runner
  tar extraction failures and deprecation warnings on Linux and macOS. Increased server
  approval test polling tolerance to ensure race detector reliability under high runner load.

## v0.6.0 — 2026-09-21 — memory in the UI

- **Memory curation in the web interface.** Added a Memory drawer (`🧠` button in the sidebar and `/memory` slash command) displaying all facts recorded in `MEMORY.md` across conversations, complete with origin run ID and timestamp, capacity counter (`X / 50 notes`), and individual deletion.
- **Forced approval invariant.** In the web UI, Ariadne's `remember` tool is strictly wired with interactive approval gating (`agentOpts.Memory = true`). The model can never record a fact unattended: every fact presented to future conversations was explicitly reviewed and approved by the operator.
- **Atomic note deletion.** Added `Store.Delete(index int, expectedText string)` in `internal/memory` with optimistic validation. Both the index and exact expected note text are verified before atomic temp-file rewrite, sync, and directory flush (with platform directory syncing for Windows and Unix), preventing race conditions or accidental deletion of the wrong note.
- **REST memory endpoints with CSRF protection.** `GET /api/memory` and `DELETE /api/memory` endpoints wired into the HTTP server with same-origin CSRF verification.
- **Persistence across resumed conversations.** `state.Memory` is recorded in checkpoint state so re-opened and resumed conversations retain memory access consistently.

## v0.5.5 — 2026-09-21 — provenance, fuzzing & modular architecture

- **Command layer modularization.** Refactored monolithic `cmd/ariadne/main.go`
  (2,320 lines) into focused, single-responsibility files in `package main`
  (`resolve.go`, `approval.go`, `traces.go`, `eval.go`, `run.go`, `chat.go`,
  `ui.go`, `agent.go`, `main.go`). Flag definitions, terminal REPL loops, trace
  querying, and agent assembly are cleanly decoupled with zero test breakage.
- **Build provenance & supply chain security.** All GitHub Actions workflows are
  pinned to immutable commit SHAs. Releases now include cryptographic GitHub
  Artifact Attestations (`actions/attest-build-provenance`) for binary verification.
  Added Dependabot configuration for weekly Go module and Action updates. Upgraded
  toolchain to Go 1.26.6, resolving all stdlib vulnerabilities.
- **Automated vulnerability audit.** Added `make audit-vuln` target running
  `govulncheck ./...`, integrated as a blocking gate in Linux CI.
- **Automated release notes extraction.** Configured `fetch-tags: true` and a
  `git cat-file -p` fallback in the release workflow so release notes are reliably
  extracted from annotated tags.
- **Native Go fuzz testing & UTF-8 hardening.** Added `testing.F` fuzz tests
  covering URL validation (`FuzzCheckWebURL`), web text extraction (`FuzzHTMLText`),
  run ID sanitization (`FuzzValidRunID`), and checkpoint unmarshaling (`FuzzCheckpointJSON`).
  Fuzzing detected and fixed an issue where invalid UTF-8 byte sequences from web
  scrapes bypassed sanitization before reaching provider APIs.

## v0.5.4 — 2026-09-21 — the cheap correctness batch

- **Stop offering a tool after three denials in one run.** Mitigates approval
  fatigue and prevents models from looping after an operator denies an action.
  After the third denial of a tool, it is dropped from the offered tools and
  any further attempt is refused automatically without asking again.
- **Terminal approval prompt timeout.** Unanswered terminal approval prompts
  now time out after 5 minutes and fail closed (denying the call), matching the
  browser UI timeout. Context cancellation (such as Ctrl-C) unblocks the prompt
  immediately without waiting for input.
- **Security test skip detection in `make check`.** Tests matching
  `Sandbox|Injection|Gate|Trust|Redact` that skip will fail `make check` unless
  the test and its justification are explicitly recorded in `scripts/audit-skips.go`
  (covering Windows and Unix platform skips).
- **`-repeat 3` in `make eval-daily`.** Runs each task 3 times in daily eval sweeps
  to prevent single-run model sampling noise from polluting scorecards.
- **Checkpoint directory fsynced on Unix.** `Store.Save` fsyncs the parent
  directory after renaming the checkpoint file, ensuring directory entries reach
  durable storage.
- **The browser tab has an icon.** A spiral — Ariadne's thread — drawn as
  an inline SVG, so the page is still one file with no build step. It
  follows light and dark, which a `.ico` cannot, and declaring it also
  stops the browser asking for `/favicon.ico` on every load and being
  told 404.

## v0.5.3 — 2026-09-20 — the download works on Windows

- **Double-clicking `ariadne.exe` on Windows opens the browser interface.**
  It used to print the list of commands into a console window that Windows
  destroyed in the same instant, so a fresh download appeared to do nothing at
  all. Only a launch with no arguments at all does this; running it from a
  terminal still prints the commands, and nothing changes on macOS or Linux.
- **The first line of `--help` says what ariadne is** — a personal AI
  assistant that asks before it acts — instead of describing a run as a job,
  which is positioning the project moved away from months ago.
- **README says what Windows will do on first run**: SmartScreen warns
  because the binaries are unsigned, with the `Get-FileHash` and
  `Unblock-File` commands to check and clear it. `SECURITY.md` says plainly
  that a checksum is not a signature.

- **Every answer says which model wrote it.** A small line under the
  answer, in new conversations and reopened ones. It is the model that
  actually served the turn, which is not always the one that was asked
  for, and not always the one the picker shows — the picker is what the
  *next* message will use, so after a switch they differ on purpose.

## v0.5.2 — 2026-09-20 — the tag that builds the archives

- **No user-visible change.** `TestSandboxWindowsDriveEscape` asserted a
  Windows rule on every platform, so `make check` had failed on Linux and
  macOS since CI existed — and the release job runs that gate before
  publishing anything, so no tag since v0.4.0 had produced a download.
  This is the first release with binaries attached: six archives and
  `checksums.txt`.

## v0.5.1 — 2026-09-20 — the gate says what it is gating

- **Delete a conversation.** The × on a conversation in the sidebar removes
  it for good — the checkpoint *and* its trace, so nothing about it is left
  in `ariadne traces` either. It asks first and cannot be undone. A
  conversation with a turn still running is refused until it stops.
- **`write_file` asks before it writes.** It was the one tool that changed
  your files without a card: `edit_file`, which touches only the text it
  names, asked, while a whole-file overwrite did not. Both now ask.
  `-trust write_file` gives back the old behaviour — and scripts that run
  unattended need it, because a gated call with no terminal to ask is
  denied, and the denial now names the flag.
- **Approval cards say what the call would do.** Instead of
  `{"new_text":"...","path":"...","old_text":"..."}`, an edit shows the file
  and the line it lands on with the old text and the new, a write says
  whether it creates a file or what it replaces and shows the first lines,
  `web_fetch` shows the whole URL, `exec` one argument per line, and a note
  to remember is shown as a sentence. The exact arguments are still one
  click away in the browser. A call that is about to be refused — text that
  is not in the file, or occurs twice — says so on the card instead of
  spending your yes.

## v0.5.0 — 2026-09-20 — useful every day

- **`edit_file` stopped failing on text the model had just read.** Every tool
  result was fenced with one newline more than the file holds, so a model that
  copied a line back into `old_text` was told "old_text was not found" — three
  times in a row in one conversation, on a file it had read correctly. The
  fence no longer adds that newline, `edit_file` forgives one extra trailing
  newline, and a replacement in a CRLF file is written with CRLF instead of
  leaving one line ending differently.
- **Formatted answers in the browser.** Headings, bold, lists, tables, code
  and links are shown formatted instead of as `**` and `##`, in new answers
  and reopened conversations. Images are never loaded (shown as text), and
  nothing in an answer is ever treated as HTML.
- **Switch between saved keys in Settings.** With OpenRouter and Gemini keys
  both set up, choosing the other provider in ⚙ with the key field empty uses
  that provider's own saved key; the field says "saved — leave empty to use
  it". A URL typed under "Other" never gets a saved key.
- **"Working…" while a turn runs**: a status line above the message box with
  the time so far and what is happening — thinking, calling a tool, waiting
  for your approval, writing the answer — so a long turn no longer looks
  stuck.
- **`write_file` refuses `.pdf`, `.docx`, `.xlsx`, `.pptx` and the like.** It
  writes plain text, so "save it as hello.pdf" made a file every PDF reader
  called damaged. It now says to save as `.md` or `.txt` instead.
- **Slash commands in the browser**: `/help`, `/new`, `/settings`,
  `/model [id]`, `/folder`, `/theme [system|light|dark]`, `/stop`. They are
  answered by the page, never sent to the model; `/help` used to be a paid
  model call that described the app from guesswork. `//text` sends a message
  that starts with a slash.
- **`list_files`**: list a folder in the workspace (folders first, then
  files with size and date), so you no longer have to paste a file's exact
  name. Read-only and confined like `fetch`; the listing is marked as outside
  text, since a file name can be anything. Large folders show 500 entries
  and the count of the rest.
- **Long documents in parts.** `fetch` used to return only the first 256 KB of
  a document's text, so a book was read up to about page 112 and the rest was
  unavailable — a model asked to summarise one summarised the rest from the
  table of contents. Text now comes in 256 KB parts (`part=2`, …), each saying
  which part it is and how to get the next, up to 16 MB of text. This applies
  to plain text files too, which were refused outright past 256 KB.
- **Set up in the browser.** `ariadne ui` no longer exits when there is no
  key: the page opens on a setup card (OpenRouter, OpenAI, Gemini, xAI,
  Ollama with its model list, or any OpenAI-compatible endpoint). The key is
  checked with one request and saved to `config.env`, like `ariadne setup`,
  and takes effect without a restart. **⚙** changes provider, key or model
  later; leaving the key empty keeps the saved one. The key is never sent
  back to the page. A setting exported in your shell still wins, and the page
  says so.
- **Theme switch** in the browser: system, light or dark, remembered in that
  browser. The page now paints its own background, so a browser's dark
  default no longer shows through the light theme.
- **Documents.** `fetch` reads the text of `.docx`, `.xlsx`, `.pptx`, `.odt`,
  `.ods` and `.odp` files: tables as tab-separated rows, spreadsheets with a
  heading per sheet and dates as dates, slides in order. PDFs are read through
  Poppler's `pdftotext` when it is installed, with the command to install it
  when it is not. Text past 256 KB is cut and marked, instead of the document
  being refused. Old binary `.doc`/`.xls`/`.ppt` files get a message to save
  them in the newer format.
- **Ollama in `setup`.** `ariadne setup -provider ollama` asks no key, lists
  the models Ollama has pulled, and checks the chosen one can call tools; a
  model that cannot, Ollama not running, or no models pulled is reported
  before anything is written. An endpoint on this machine no longer needs a
  key to start. The setup check now offers a tool, for every provider.
- **Unknown cost says "unknown".** A provider that reports no cost (anything
  but OpenRouter) used to show `cost=0.0000`, as if the conversation were free.
  The terminal now prints `cost=unknown`, the browser `cost unknown`, and
  `traces -stats` counts the unpriced responses; a total missing some steps is
  shown as a lower bound (`>=$…`). A free model that reports a cost of zero
  still shows as free. The run summary now prints the dollar sign (`cost=$0.0003`).
- **`edit_file`**: change an exact piece of text in a file and leave the rest
  byte for byte. The text must occur once (or `replace_all`); a miss or an
  ambiguous match is refused with the count. Keeps Windows (CRLF) line endings
  and the file's permissions, writes atomically, stays inside the workspace,
  and asks first unless `-trust edit_file`.
- **`web_fetch`**: read a web page by URL. It asks before every fetch, with the
  whole URL shown, unless `-trust web_fetch`; refuses your own machine and
  local network (loopback, private, link-local, carrier-grade NAT) on every
  connection, including after redirects; sends no cookies or keys; strips
  scripts, styles and comments; caps what it returns.
- **Stop** in the browser ends the running turn — mid-answer or while a tool call
  waits for approval. What already ran is kept; a call that was waiting is asked
  again on Resume.
- **Resume turn** in the browser finishes a turn that was interrupted (a closed
  tab, a crash, Stop). Before, such a conversation answered every new message
  with an error.
- A conversation is saved as soon as a turn starts, so stopping or crashing
  during the first answer no longer loses the question.
- A failing folder dialog now says so instead of looking like a cancel.

## v0.4.0 — 2026-09-19 — install it in five minutes

- **`ariadne setup`**: pick a provider (OpenRouter by default), enter its key
  with the input hidden, and it checks the key with one small request before
  saving anything.
- **One home for your data.** Conversations, the default workspace, `MEMORY.md`
  and `config.env` live in your user config folder (`%AppData%\ariadne` on
  Windows, `~/Library/Application Support/ariadne` on macOS,
  `~/.config/ariadne` on Linux), whatever folder you run ariadne from.
  `ARIADNE_HOME` moves it. Inside a source checkout, the checkout is the home,
  as before.
- **OpenRouter is the default endpoint.**
- **`ariadne ui` opens your browser** (`-no-open` to skip).
- **Choose a folder per conversation in the browser.** **Folder…** before the
  first message: type a full path or **Browse…** for your system's folder
  dialog. The folder is fixed once the conversation starts and shown at the
  top. `ariadne ui -workspace` is now only the default for new
  conversations; a reopened one always keeps its own folder.
- **`ariadne version`.**
- **Release binaries** for Windows, macOS and Linux (amd64 and arm64), and CI
  on all three.
- A provider's key is only ever sent to that provider's own host. Any other
  endpoint (Groq, Together, Ollama, ...) uses `ARIADNE_API_KEY`.
- A `.env` in the folder you run from is no longer read, unless it is an
  ariadne source checkout — it was somebody else's secrets.
- A closed browser tab no longer counts as "deny": the tool call waits, and is
  asked again when the conversation resumes.
- Fixed: `ariadne traces` failed for every run if any one trace had a line
  over 8 MB; such a line is now skipped and counted as malformed.
- Fixed: on Windows, input redirected from `NUL` was taken for a terminal.
- Fixed: `.env` values ending in a quote lost it.

## v0.3.0 — 2026-09-17 — it can do work, and the gate is real

- `-workspace`: point the file tools at a folder you choose.
- `-mcp-config`: MCP servers over stdio. Tools are named `<server>__<tool>`,
  and every MCP tool asks for approval unless listed in `-trust`.
- Approval in both the terminal and the browser.
- `exec` (with `-exec`): runs a program by argv, no shell. Always asks. Not
  confined to the workspace.
- ariadne's own API keys are redacted from every tool result.

## v0.2.0 — 2026-09-15 — a chat window

- `ariadne chat` in the terminal and `ariadne ui` in the browser, over the same
  conversations: start in one, finish in the other.
- Pick the model from a list; conversations keep the model they started with.

## v0.1.0 — 2026-09-13 — a run is a job

- `run`, `resume`, `eval`, `traces`.
- A checkpoint after every tool call; resume after `kill -9` without repeating
  a tool that already ran.
- Per-commit scoring on a fixed task set.
- Tools confined with `os.Root`, untrusted content fenced, an allow-list and
  per-call approval.
