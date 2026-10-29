# Multi-provider support with a shared AGENTS.md — Design

Date: 2026-10-21 · Status: approved

## Intent

tf-architect currently only works with the Claude Code CLI in `--bare` mode, which
requires `ANTHROPIC_API_KEY`. The user works with a **personal Claude
subscription (login)**. The tool must:

- work with the Claude login (Claude only);
- accept an API key for any provider (Anthropic, OpenAI, Gemini, and
  OpenAI-compatible endpoints);
- put **security first**: no sensitive data ever reaches any provider, and
  if isolation cannot be guaranteed or verified the run stops and **shows
  the reason in a pop-up** (a prominent terminal panel);
- use **one provider-neutral `AGENTS.md`** as the project's single source of
  truth: it is the generic prompt for every provider, and it is always
  written into the generated project.

## Goals

1. `--provider` chooses the backend:
   - `claude` (default): Claude Code CLI using the user's login (an
     `ANTHROPIC_API_KEY` in the environment also works, the CLI decides).
   - `anthropic`, `openai`, `gemini`: direct HTTPS API with an API key.
   - `openai-compatible`: direct API to a user-supplied `--base-url`.
2. Every provider follows the same pipeline, prompts and security model.
3. `prompts/AGENTS.md` (embedded, replaces `prompts/system.md`) is the system
   prompt for every provider. It is copied into the generated project's root
   with a project-specific section appended.
4. No AI-side artifacts are left behind: no transcripts, no CLI session
   files, no temporary dirs.

## Non-goals

- Login for OpenAI, Gemini or any provider other than Claude.
- Letting the model run commands, `terraform plan/apply`, or use any tool.
- Generating `CLAUDE.md`, `GEMINI.md` or other provider-specific files.

## Anti-goals (failure even if it "works")

- Any unmasked byte reaching any provider, including user `CLAUDE.md`,
  memories, hooks, plugins or MCP servers loaded by the Claude CLI.
- Deleting or modifying the user's own configuration (hooks, settings,
  plugins). They are **isolated**, never deleted.
- Sending data after an isolation check failed, or failing silently.

## Design

### 1. The model never gets tools: a single file protocol (my call)

Today Claude writes files itself through file tools. Instead, **no provider
gets any tool** (`--tools ""` for the Claude CLI; plain completions for the
APIs). The model returns files in a strict text protocol, and tf-architect
parses, validates and writes them:

```
===FILE: modules/vpc/main.tf===
<full file content>
===END FILE===
===DELETE: old/unused.tf===
```

- Paths must be relative, clean, without `..`, outside `.terraform/`,
  `.git/` and `.tf-architect/`, with an allowed extension (`.tf`,
  `.tfvars`, `.md`, `.json`, `.tpl`, `.yaml`, `.yml`, `.sh`). Any other path
  aborts the step (pop-up).
- A response that opens a `FILE` block without closing it counts as
  truncated: retry the step once, then stop with a pop-up.
- Edits (doc-changed, auto-fix) send the **current masked files** in the
  prompt, and the model returns whole replacement files.

Why: the security guarantee no longer depends on how each CLI or vendor
confines its tools. Only the masked prompt text leaves the process, and
nothing the model says is executed. One code path serves every provider.
The `outputDir` checks stay as defence in depth.

### 1b. Real values on disk, masking at the boundary (my call)

With no model tools, the model can only see what tf-architect puts in a
prompt. So `outputDir` **always holds real values**:

- **Outbound:** file contents placed in a prompt go through the gate
  (masked) like any other text.
- **Inbound:** files returned by the model are de-masked (`UnmaskText`)
  before they are written.

This removes `MaskDir`/`Unmask` over the directory, the de-masked
validation copy, the "files stay masked if interrupted" state, and the
deferred unmask. Validators run directly on `outputDir`. The masker, the
mask map and the gate stay as they are.

### 2. Stateless calls with explicit context (my call)

Drop `--resume`. Every call sends: system prompt (`AGENTS.md` + masking
note + file-protocol note), the current session context blob (analysis,
clarifications, file list, last validation), and the task prompt. This
works the same for APIs (stateless by nature) and for the Claude CLI with
`--no-session-persistence`. It costs more tokens and removes a class of
resume bugs.

### 3. Generation in steps

Output-token limits make "whole project in one answer" unreliable on APIs.

1. **Plan call:** the model returns JSON `{"modules":[{"name","files":[...]}]}`.
2. **One call per module:** returns that module's files in the protocol.
   Each completed module is a checkpoint (`GenerateStep`), so an
   interrupted run resumes at the next module.
3. Root files (`versions.tf`, `providers.tf`, `main.tf`, …) form the last
   module, written knowing every module's outputs.
4. **Whole-project review (user decision):** once all modules are
   written, one call receives the complete masked project. It reviews
   cross-module consistency (module inputs and outputs, references,
   variables, golden rules, naming) and returns only corrected files in the
   protocol, or `NO_CHANGES`. It is its own checkpoint (`GenerateStep:
   "review"`), so a resumed run does not repeat it. Validation runs after
   it.

Auto-fix and doc-changed send the relevant masked files (all project files
up to a size budget). If the project exceeds the budget, stop with a pop-up
that names the limit. No silent truncation.

### 4. Providers

`internal/llm` defines `Provider.Complete(ctx, system, prompt) (string, error)`.
Every implementation is wrapped by the existing gate (interrupt check + mask)
in the client, so there is still a single choke point.

| Provider | Transport | Credentials | Model |
|---|---|---|---|
| `claude` | `claude -p` subprocess | login or `ANTHROPIC_API_KEY` | CLI default, or `--model` |
| `anthropic` | HTTPS Messages API | `ANTHROPIC_API_KEY` | `--model`, default the latest Sonnet |
| `openai` | HTTPS Chat Completions | `OPENAI_API_KEY` | `--model` required |
| `gemini` | HTTPS generateContent | `GEMINI_API_KEY` | `--model` required |
| `openai-compatible` | HTTPS Chat Completions at `--base-url` | `TFA_API_KEY` | `--model` required |

- API keys are read **only from the environment**, never from flags
  (shell history, `ps`). They are never logged or persisted.
- Plain `net/http` + `encoding/json`, no vendor SDKs (my call: three small
  JSON APIs; fewer dependencies to audit).
- HTTPS only. `openai-compatible` with `http://` is refused, except for
  `localhost`.

### 5. Claude CLI isolation (login mode)

Flags: `-p --output-format stream-json --verbose --safe-mode
--no-session-persistence --restricted --strict-mcp-config
--setting-sources "" --tools "" --permission-prompts none --system-prompt …`.
Env: `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`.
cwd: a fresh empty temp dir (it no longer needs `outputDir`).

Verified on Claude Code 2.1.306 (2026-10-21): login works, no `CLAUDE.md`
or memory reaches the model, user plugins and hooks are not loaded, and no
transcript is written to `~/.claude/projects`.

**Isolation verification.** The `system/init` event reports `tools`,
`mcp_servers`, `plugins` and `cwd`. Before any real data is sent, each run
makes one **preflight call** with a harmless prompt and checks:

- `tools` is empty, `mcp_servers` is empty;
- every plugin's `path` is `builtin`;
- `cwd` is the temp dir;
- the CLI version is at least the verified minimum.

Each real call also checks its own init event. A mismatch aborts that call:
its result is discarded and the run stops with a pop-up. (The prompt of
that call has already left, but it is masked; the preflight exists so this
can never happen on the first real call.) If `--safe-mode` or the other
flags are not supported, the preflight fails → pop-up.

### 6. Pop-up on isolation or verification failure

A boxed, high-contrast red terminal panel (my call: terminal panel, no OS
notification), printed **before** stopping. It contains:

- what failed (e.g. "Claude CLI loaded user plugin `caveman`");
- why it matters (what could have been sent);
- what to do (upgrade the CLI, use `--provider anthropic`, …);
- the confirmation that nothing sensitive was sent.

It is used for every fail-closed abort: masking or `mask.json` errors,
failed preflight or init checks, unsafe paths from the model, truncation
after the retry, an over-budget project, an unsafe `--base-url`, and an
`outputDir` containing the state or the document. For `openai-compatible`
with a non-local host, the panel is a warning that needs an explicit `y` to
continue (default No). Non-interactive runs refuse unless
`--yes-custom-endpoint` is passed.

### 7. AGENTS.md as the single source of truth

- `prompts/AGENTS.md` (embedded) replaces `prompts/system.md`. It is
  rewritten in provider-neutral language: no tool names. It covers the
  golden rules, the module layout, the file protocol, the self-review rule
  ("nothing is executed; the end user validates"), and a short "how to work
  on this repo" section for future agents.
- `--system-file` is renamed `--agents-file`. It replaces the embedded
  `AGENTS.md` for the model and for the copy written into the project.
- At the end of the docs phase, `<outputDir>/AGENTS.md` is written as the
  generic content plus a generated **Project** section: architecture
  summary, decisions from clarifications, module map and conventions. The
  model writes it with placeholders, and it is de-masked on write like every
  other file.
- The file opens with a note that it contains real infrastructure values
  once de-masked, so any AI tool later opened on the repo will read them
  (the user's responsibility, same as the `.tf` files).

### 8. Cleanup

- The Claude CLI temp cwd is removed via `defer`.
- After the run, check that `~/.claude/projects/<slug of temp cwd>` does not
  exist. If it does (a CLI that ignored `--no-session-persistence`), delete
  it, since it is ours, and warn.
- User hooks, settings, plugins and memories are never touched.

## Constraints

- Go, cobra, existing package layout (`internal/claude` becomes the `claude`
  provider inside `internal/llm`).
- Keep the masking guarantees and the end-to-end no-leak test, extended to
  every provider with HTTP test servers (`httptest`) and the stub CLI.
- No reengineering beyond what the file protocol requires. The FSM phases
  stay the same.

## Left to the builder

- Exact prompt wording, size budget, and retry/backoff on HTTP 429 and 5xx.
- Default Anthropic model id, checked against current docs at build time.
- Pop-up rendering details (width, wrapping, `NO_COLOR` fallback).
- Whether streaming is used for APIs. Not required, since generation is per
  module.
