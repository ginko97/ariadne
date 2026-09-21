# Changelog

What changed for someone using ariadne. The tag messages (`git show v0.3.0`)
carry the longer story for each release.

## Unreleased

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
  the test and its justification are explicitly recorded in `scripts/audit-skips.go`.
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
