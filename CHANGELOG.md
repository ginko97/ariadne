# Changelog

What changed for someone using ariadne. The tag messages (`git show v0.3.0`)
carry the longer story for each release.

## Unreleased

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
