# Changelog

What changed for someone using ariadne. The tag messages (`git show v0.3.0`)
carry the longer story for each release.

## Unreleased

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
