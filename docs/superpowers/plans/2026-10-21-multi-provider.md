# Multi-provider + AGENTS.md Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run tf-architect with the Claude login or with any provider's API key. The model gets no tools, uses a single file protocol, writes one shared `AGENTS.md`, and every fail-closed security stop shows a pop-up.

**Architecture:** A new `internal/llm` package holds a `Provider` interface (Claude CLI, Anthropic, OpenAI, OpenAI-compatible, Gemini) behind one gated `Client`, the single point where prompts are masked and interrupts are checked. Models return files through `internal/fileproto`. The orchestrator writes those files de-masked, so `outputDir` always holds real values and validators run on it directly. Generation runs as a plan call, one call per module, then a whole-project review.

**Tech Stack:** Go 1.26, cobra, `net/http` + `encoding/json` (no vendor SDKs), `httptest` for tests.

**Spec:** `docs/superpowers/specs/2026-10-21-multi-provider-design.md`

## Global Constraints

- The model never gets tools. Claude CLI flags, exactly: `-p --output-format stream-json --verbose --safe-mode --no-session-persistence --restricted --strict-mcp-config --setting-sources "" --tools "" --permission-prompts none --system-prompt <s>`, plus `--model <m>` when set.
- Claude CLI env: `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`. cwd: a fresh `os.MkdirTemp` dir, removed after.
- Minimum verified Claude CLI version: `2.1.306`.
- API keys come only from env: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `TFA_API_KEY` (openai-compatible). They are never logged, persisted or taken from flags.
- Base URLs: HTTPS only, except `http://localhost`, `http://127.0.0.1` and `http://[::1]`. Any `--base-url` with a non-local host needs a `y` confirmation (default No), or `--yes-custom-endpoint` when non-interactive.
- File protocol: `===FILE: <path>===` … `===END FILE===`, `===DELETE: <path>===`, and `NO_CHANGES` for an empty review.
- Allowed file extensions: `.tf .tfvars .md .json .tpl .yaml .yml .sh`. Forbidden path segments: `..`, `.terraform`, `.git`, `.tf-architect`. No absolute paths.
- Prompt task headers, exact (tests key on them): `## TASK: ANALYZE`, `## TASK: CLARIFY`, `## TASK: PLAN`, `## TASK: MODULE <name>`, `## TASK: REVIEW`, `## TASK: ADJUST`, `## TASK: FIX`, `## TASK: DOCS`.
- Project size budget sent in one prompt: `fileproto.DefaultBudget = 400_000` bytes.
- Every pop-up ends with the line: `Nothing sensitive was sent to the AI provider.`
- Never touch the user's hooks, settings, plugins or memories.

## Review Focus

- The model returns a placeholder that is not in the mask map (e.g. `CIDR_PRIVATE_999`): write it as-is and warn, naming the file and the placeholder. Test: Task 6 `TestUnknownPlaceholderWarns`.
- A response with prose, CRLF line endings or ``` fences around blocks: ignore text outside blocks, normalise CRLF, and strip one fence pair that wraps a block's content. Test: Task 2 `TestParseTolerant`.
- The same path appears twice in one response: last block wins and a warning is returned. Test: Task 2 `TestParseDuplicatePathLastWins`.
- HTTP 429/5xx from a provider: retry up to 3 attempts with backoff (1s, 2s; injectable for tests), then return an error. Not a pop-up. Test: Task 4 `TestRetryOn429`.
- Ctrl-C during an HTTP call: the request context is cancelled and the call returns promptly. Test: Task 4 `TestCancelAbortsRequest`.

---

### Task 0: Commit the existing hardening work

- [ ] **Step 1:** Run `go test ./... -count=1`. Expected: all `ok`.
- [ ] **Step 2: Commit**

```bash
git add -A ':!.rtk' && git commit -m "fix: harden masking, confine Claude, best-effort validation, custom sensitive values"
```

### Task 1: Security error and pop-up

**Files:**
- Create: `internal/security/error.go`, `cmd/popup.go`, `cmd/popup_test.go`, `internal/security/error_test.go`

**Interfaces:**
- Produces: `type Error struct{ What, Why, Fix string }`, `func New(what, why, fix string) *Error`, `(*Error) Error() string` returning `"security stop: " + What`. In `cmd`: `func renderPopup(e *security.Error) string`.

- [ ] **Step 1: Write failing tests.** `TestErrorsAs`: wrapping with `fmt.Errorf("x: %w", New(...))` still matches `errors.As`. `TestRenderPopup`: the output contains What, Why, Fix and `Nothing sensitive was sent to the AI provider.`; with `colorEnabled=false` it has no `\033`; every line has the same display width (`utf8.RuneCountInString`).
- [ ] **Step 2:** `go test ./internal/security ./cmd`. Expected: FAIL (undefined).
- [ ] **Step 3:** Implement. Box with `┌─┐│└┘`, title `SECURITY STOP`, width 72, words wrapped. Red bold when `colorEnabled`.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(security): typed security stop and terminal pop-up`.

### Task 2: File protocol

**Files:**
- Create: `internal/fileproto/fileproto.go`, `internal/fileproto/fileproto_test.go`

**Interfaces:**
- Consumes: `security.New`, `fsutil.WalkFiles`.
- Produces:
  - `type Change struct{ Path, Content string; Delete bool }`
  - `var ErrTruncated = errors.New("response truncated: FILE block not closed")`
  - `const NoChanges = "NO_CHANGES"`, `const DefaultBudget = 400_000`
  - `func Parse(resp string) (changes []Change, warnings []string, err error)`
  - `func ValidatePath(p string) error`, which returns `*security.Error`
  - `func Apply(dir string, changes []Change, transform func(string) string) error`, where `transform` is applied to `Content` before writing (0644 files, 0755 dirs) and paths are re-checked with `ValidatePath`
  - `func ReadProject(dir string, budget int) (map[string]string, error)`: allowed extensions only, relative paths, `*security.Error` over budget
  - `func Render(files map[string]string) string`: blocks sorted by path

- [ ] **Step 1: Write failing tests.**
  - `TestParseBasic`: two FILE blocks and one DELETE give 3 changes in order.
  - `TestParseNoChanges`: `"NO_CHANGES"` → 0 changes, nil error.
  - `TestParseTruncated`: an unclosed block gives `errors.Is(err, ErrTruncated)`.
  - `TestParseTolerant`: prose before and after, CRLF, and content wrapped in ```hcl … ``` all parse; the content has no fence and no `\r`.
  - `TestParseDuplicatePathLastWins`: one change with the second content and 1 warning.
  - `TestValidatePathRejects`: each of `/etc/x.tf`, `../x.tf`, `a/../../x.tf`, `.terraform/m.tf`, `.git/config.tf`, `.tf-architect/s.json`, `x.exe` and `` gives an `*security.Error`. `modules/vpc/main.tf` gives nil.
  - `TestApplyTransformAndDelete`: the transform upper-cases the content, the file is written, and DELETE removes an existing file.
  - `TestReadProjectBudget`: over budget gives a `*security.Error`. Files under `.terraform/` and `x.bin` are excluded.
  - `TestRenderRoundTrip`: `Parse(Render(m))` reproduces m.
- [ ] **Step 2:** `go test ./internal/fileproto`. Expected: FAIL.
- [ ] **Step 3:** Implement. Scan line by line. A block starts at a line exactly `===FILE: p===` and ends at `===END FILE===`. Clean paths with `filepath.ToSlash(filepath.Clean)` before checking.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(fileproto): strict file protocol for model output`.

### Task 3: llm core and Claude CLI provider

**Files:**
- Create: `internal/llm/llm.go`, `internal/llm/claudecli.go`, `internal/llm/claudecli_test.go`, `internal/llm/llm_test.go`
- Delete: `internal/claude/` (behaviour moves here)

**Interfaces:**
- Consumes: `security.New`.
- Produces:
  - `type Provider interface{ Name() string; Complete(ctx context.Context, system, prompt string) (string, error) }`
  - `type Preflighter interface{ Preflight(ctx context.Context) error }`
  - `type Client struct{…}`, `func NewClient(p Provider, system string) *Client`, `func (c *Client) SetGate(fn func(string) (string, error))`, `func (c *Client) Query(ctx context.Context, prompt string) (string, error)`. The gate runs on the system prompt and the prompt; a gate error means `Complete` is never called.
  - `type ClaudeCLI struct{ Bin, Model, ConfigDir string }`. `Bin` defaults to `"claude"`. `ConfigDir` defaults to `$CLAUDE_CONFIG_DIR`, else `~/.claude`.
  - `func (c *ClaudeCLI) Preflight(ctx) error` sends `"Reply with OK."` and checks the init event.
  - `func checkInit(ev map[string]any, wantCwd string) error` returns a `*security.Error` naming the first violation.
  - `const MinClaudeVersion = "2.1.306"`.

- [ ] **Step 1: Write failing tests.** Use a helper-process stub: `Bin` points to a script that runs the test binary with `-test.run=TestHelperClaude` and `GO_HELPER=1`. The helper writes its args and env to a file and prints events chosen by env `HELPER_INIT`.
  - `TestClaudeArgsAndEnv`: exact Global-Constraints flags and env; cwd is not the caller's cwd and is gone after `Complete`.
  - `TestCheckInit`: violation cases are tools `["Read"]`, an mcp server, plugin path `/home/u/.claude/plugins/x`, a cwd mismatch, and version `2.1.100`. Each gives `*security.Error`; the clean init gives nil.
  - `TestCompleteDiscardsOnBadInit`: text after a bad init is not returned and an error is returned.
  - `TestTranscriptCleanup`: the helper creates `<ConfigDir>/projects/<slug(cwd)>/x.jsonl`, `Complete` removes it, and the error is nil.
  - `TestClientGateBlocks`: a gate error means the provider spy got no call.
  - `TestClientGateApplied`: the provider sees the gated system and prompt.
- [ ] **Step 2:** `go test ./internal/llm`. Expected: FAIL.
- [ ] **Step 3:** Implement. Reuse the stream-json reader from the old `internal/claude/client.go`: drain stdout, always `Wait`, stderr tail in errors. `slug(p)` replaces every non-alphanumeric rune with `-`. Compare versions numerically per dot segment.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(llm): provider interface, gated client, isolated Claude CLI`.

### Task 4: HTTP providers and factory

**Files:**
- Create: `internal/llm/http.go`, `internal/llm/anthropic.go`, `internal/llm/openai.go`, `internal/llm/gemini.go`, `internal/llm/factory.go`, `internal/llm/http_test.go`, `internal/llm/factory_test.go`

**Interfaces:**
- Produces:
  - `type Config struct{ Provider, Model, BaseURL string; Getenv func(string) string; ClaudeBin string }`
  - `func New(cfg Config) (Provider, error)`. Provider names are `claude`, `anthropic`, `openai`, `gemini`, `openai-compatible`.
  - `func IsLocalURL(raw string) (bool, error)`, which errors on a non-HTTPS non-local URL.
  - `var retryDelays = []time.Duration{time.Second, 2 * time.Second}` (tests override it).
- Endpoints (default base URLs):
  - Anthropic `POST https://api.anthropic.com/v1/messages`, headers `x-api-key`, `anthropic-version: 2023-06-01`, body `{model,max_tokens:32000,system,messages:[{role:user,content}]}`.
  - OpenAI `POST https://api.openai.com/v1/chat/completions`, `Authorization: Bearer`, messages `system`+`user`.
  - Gemini `POST https://generativelanguage.googleapis.com/v1beta/models/<model>:generateContent`, header `x-goog-api-key`, `systemInstruction` + `contents`.
  - openai-compatible: OpenAI shape at `<BaseURL>/chat/completions`.
- Default Anthropic model: the latest Sonnet id per the current Anthropic docs (check with the claude-api skill at build time). `--model` is required for `openai`, `gemini` and `openai-compatible`.

- [ ] **Step 1: Write failing tests** with `httptest.NewServer` and `BaseURL` set to it.
  - `TestAnthropicRequestShape`, `TestOpenAIRequestShape`, `TestGeminiRequestShape`: headers, path, the system prompt in the right field, and the parsed text returned.
  - `TestRetryOn429`: 429, 500, then 200 gives success after 3 hits. Four failures give an error.
  - `TestCancelAbortsRequest`: the server blocks; cancelling ctx returns in under 1s.
  - `TestKeyNeverInError`: on a 401 the error text does not contain the key.
  - `TestNewRequiresKeyAndModel`: a missing env key or a missing `--model` gives an error naming the env var or flag.
  - `TestIsLocalURL`: `http://localhost:8080` → true; `https://x.com` → false; `http://x.com` → error.
- [ ] **Step 2:** `go test ./internal/llm`. Expected: FAIL.
- [ ] **Step 3:** Implement. Shared `postJSON(ctx, url, headers, body, out) error` with retry on 429 and 5xx, `http.Client{Timeout: 10 * time.Minute}`.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(llm): Anthropic, OpenAI(-compatible) and Gemini API providers`.

### Task 5: AGENTS.md as the generic prompt

**Files:**
- Create: `prompts/AGENTS.md`
- Delete: `prompts/system.md`
- Modify: `prompts/prompts.go` (`//go:embed AGENTS.md`, `var Agents string`), `prompts/prompts_test.go` (create)

- [ ] **Step 1: Write a failing test.** `TestAgentsProviderNeutral`: `Agents` contains `GOLDEN RULES`, `===FILE:`, `===END FILE===`, `NO_CHANGES` and `the end user`. It does not contain `Write tool`, `Bash`, `Claude` or `STEP_COMPLETE`.
- [ ] **Step 2:** `go test ./prompts`. Expected: FAIL.
- [ ] **Step 3:** Write `prompts/AGENTS.md`. Port the 10 golden rules and the layout from `system.md` in provider-neutral wording, and add these sections: Output protocol (the exact markers), Self-review (nothing is executed; the end user validates and tests), Masked values (the current `maskingNote` text, moved here), and Working on this repository (for future agents).
- [ ] **Step 4:** Run the test. Expected: PASS.
- [ ] **Step 5:** Commit `feat(prompts): provider-neutral AGENTS.md replaces system.md`.

### Task 6: Orchestrator on llm.Client with real values on disk

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`, `internal/orchestrator/resume.go`, `internal/masking/masker.go`, `internal/masking/masker_test.go`
- Delete: `internal/orchestrator/validate.go`
- Test: `internal/orchestrator/orchestrator_test.go`

**Interfaces:**
- Consumes: `llm.Client`, `llm.Preflighter`, `fileproto.*`, `security.New`.
- Produces:
  - `func New(workDir, outputDir, agents string, p llm.Provider, noMask bool) *Orchestrator`
  - `(*Orchestrator) Interrupt()` cancels the internal ctx.
  - `(*Orchestrator) applyChanges(changes []fileproto.Change) error` de-masks via `UnmaskText` and warns on unknown placeholders.
  - `func masking.UnresolvedPlaceholders(text string, mm MaskMap) []string`
- Removes: `masking.MaskDir`, `masking.Unmask`, `rewriteDir`, `isBinary` and their tests; `maskOutput`, `unmaskOutput`, `validationDir`; `Client.SetContextBlob`. The context blob from `buildContextBlob(sess)` is prepended to every query by the orchestrator.

- [ ] **Step 1: Write failing tests.**
  - Rewrite the e2e stub: `fakeModel(system, prompt string) string` dispatches on the Global-Constraints task headers and is served by a fake `Provider` that records each `(system, prompt)` pair.
  - `TestRunNeverSendsSensitiveDataToClaude` → rename to `TestRunNeverSendsSensitiveData`. Same secrets list and same doc-changed second run. Assert the recorded pairs contain no secret, and the written files contain the real values.
  - `TestUnknownPlaceholderWarns`: the model returns `CIDR_PRIVATE_999`; a status line names the file and the placeholder.
  - `TestPreflightRunsBeforeFirstQuery`: a provider implementing `Preflighter` that fails means zero `Complete` calls and `errors.As(err, *security.Error)`.
  - `TestValidatorsSeeRealValues`: the stub terraform log contains the real CIDR.
  - Keep `TestRunValidationIssuesDoNotBlockDocs`, `TestRunInterruptedSendsNothing` and `TestRunRefusesOutputDirContainingState` (now a `*security.Error`).
- [ ] **Step 2:** `go test ./internal/orchestrator ./internal/masking`. Expected: FAIL.
- [ ] **Step 3:** Implement.
  - Gate = `outgoing` (interrupt + `maskText`).
  - Masking errors and `checkOutputDir` return `*security.Error`.
  - Run `Preflight` right after `initMasking`.
  - Analyze and clarify use `## TASK: ANALYZE` / `## TASK: CLARIFY`. The clarify task prompt includes the analysis JSON.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit `refactor(orchestrator): llm.Client, file protocol, real values on disk`.

### Task 7: Stepwise generation and whole-project review

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`, `internal/orchestrator/prompts.go`, `internal/state/session.go`
- Test: `internal/orchestrator/prompts_test.go`, `internal/orchestrator/orchestrator_test.go`

**Interfaces:**
- Produces:
  - `type ModulePlan struct{ Name string \`json:"name"\`; Files []string \`json:"files"\` }`
  - `type GenerationPlan struct{ Modules []ModulePlan \`json:"modules"\` }`
  - `func ParseGenerationPlan(raw string) (GenerationPlan, error)`. A plan without a last module named `root` gets one appended with `versions.tf providers.tf main.tf variables.tf outputs.tf data.tf`. `extractJSON(raw string) string` is shared with `ParseAnalysisJSON`.
  - `Session.GenerationPlan string \`json:"generation_plan,omitempty"\``
  - `(*Orchestrator) completeFiles(ctx, prompt string) ([]fileproto.Change, error)`: on `ErrTruncated` it retries once; a second truncation returns `*security.Error`.
  - Review step name: `"review"`.

- [ ] **Step 1: Write failing tests.**
  - `TestParseGenerationPlanAppendsRoot` and `TestParseGenerationPlanKeepsRootLast`.
  - `TestGenerateCallsPerModuleThenReview`: a plan of `vpc` and `rds` gives task headers, in order, `PLAN`, `MODULE vpc`, `MODULE rds`, `MODULE root`, `REVIEW`. Each MODULE prompt contains the files already written (masked).
  - `TestResumeSkipsCompletedModules`: a session with `GenerateStep: "rds"` gives headers `MODULE root`, `REVIEW` only.
  - `TestResumeSkipsDoneReview`: `GenerateStep: "review"` sends no generation headers.
  - `TestTruncatedTwiceStops`: the model always returns an unclosed block, giving 2 calls and a `*security.Error`.
  - `TestReviewNoChanges`: `NO_CHANGES` leaves files untouched.
- [ ] **Step 2:** `go test ./internal/orchestrator`. Expected: FAIL.
- [ ] **Step 3:** Implement.
  - The PLAN prompt asks for JSON only, in the `GenerationPlan` shape.
  - Each MODULE prompt carries the plan, the module's file list and `Render(ReadProject(outputDir, DefaultBudget))`.
  - The REVIEW prompt carries the whole rendered project and asks for corrected files only, or `NO_CHANGES`.
  - Checkpoint after each module and after the review via `GenerateStep`. `GeneratedFiles` is refreshed after the review.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(orchestrator): plan, per-module generation and whole-project review`.

### Task 8: Doc-changed adjust and auto-fix through the file protocol

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`
- Test: `internal/orchestrator/orchestrator_test.go`

- [ ] **Step 1: Write failing tests.**
  - `TestDocChangedUsesAdjustThenReview`: headers `ADJUST`, `REVIEW`. The ADJUST prompt contains the rendered project masked and the diff summary.
  - `TestAutoFixUsesFixWithProject`: a failing validation gives a `FIX` prompt with the masked errors and the rendered project. The returned files are applied de-masked.
  - `TestOverBudgetStops`: a 500 KB `.tf` in `outputDir` gives a `*security.Error` and no `ADJUST` call.
- [ ] **Step 2:** `go test ./internal/orchestrator`. Expected: FAIL.
- [ ] **Step 3:** Implement. Doc-changed replaces the per-module path with ADJUST, then the review. FIX replaces the old auto-fix query. Both use `completeFiles` and `applyChanges`.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(orchestrator): adjust and auto-fix via file protocol`.

### Task 9: Docs and the project AGENTS.md

**Files:**
- Modify: `internal/docs/architectural.go`, `internal/docs/generator.go`
- Create: `internal/docs/architectural_test.go`

**Interfaces:**
- Produces:
  - `type Client interface{ Query(ctx context.Context, prompt string) (string, error) }`
  - `func Generate(ctx context.Context, client Client, sess *state.Session, outputDir, agentsBase, project string, unmask func(string) string, onStatus func(string))`. `project` is the rendered masked project.
  - New section markers `===BEGIN_PROJECT_AGENTS===` / `===END_PROJECT_AGENTS===`.
  - Every file the docs phase writes goes through `unmask`.
  - `<outputDir>/AGENTS.md` = `agentsHeader` + `agentsBase` + `"\n\n## Project\n\n"` + the unmasked section. `agentsHeader` begins with `> This file contains real infrastructure values.`

- [ ] **Step 1: Write failing tests.**
  - `TestDocsWritesAgentsMD`: a fake client returns all sections with `CIDR_PRIVATE_001`, and `unmask` maps it to `10.0.0.0/16`. `AGENTS.md` exists, starts with the header, contains `agentsBase` and `10.0.0.0/16`, and no file contains `CIDR_PRIVATE_001`.
  - `TestDocsPromptHasTaskHeaderAndProject`: the prompt contains `## TASK: DOCS` and the `project` string.
  - `TestChangelogNotOverwritten`.
- [ ] **Step 2:** `go test ./internal/docs`. Expected: FAIL.
- [ ] **Step 3:** Implement. When the section is missing, still write `AGENTS.md` with header and base, and warn.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(docs): project AGENTS.md as single source of truth`.

### Task 10: CLI wiring, README and end-to-end provider matrix

**Files:**
- Modify: `cmd/main.go`, `README.md`
- Create: `cmd/main_e2e_test.go`

**Interfaces:**
- Consumes: `llm.New`, `llm.IsLocalURL`, `renderPopup`, `orchestrator.New`.
- Flags: `--provider` (default `claude`), `--model`, `--base-url`, `--yes-custom-endpoint`, `--agents-file` (replaces `--system-file`).

- [ ] **Step 1: Write failing tests.**
  - `TestProviderMatrixNoLeak` runs `runGenerate` through each provider with the env key set and `BaseURL` pointing at an `httptest` fake model: `anthropic`, `openai`, `gemini`, `openai-compatible`. `claude` uses the helper-process stub from Task 3. The test asserts no secret appears in any request body or CLI arg.
  - `TestCustomEndpointNeedsConfirmation`: a non-local `--base-url` without a TTY and without `--yes-custom-endpoint` gives a `*security.Error` and no request.
  - `TestSecurityErrorRendersPopup`: `main`'s error path prints `renderPopup` output for a `*security.Error`, and a plain message otherwise.
- [ ] **Step 2:** `go test ./cmd`. Expected: FAIL.
- [ ] **Step 3:** Implement.
  - Build the provider before `orch.Run`.
  - Confirm a custom endpoint through the shared `stdin` reader.
  - In `main`, `errors.As` to a `*security.Error` → print the pop-up to stderr, exit 2. Other errors exit 1.
  - README: providers table, env vars, login isolation, file protocol, `AGENTS.md`, cleanup, and the updated security invariant. Remove the `--bare`/`ANTHROPIC_API_KEY` requirement.
- [ ] **Step 4:** `go vet ./... && go test ./... -count=1`. Expected: all `ok`. `gofmt -l .` is empty.
- [ ] **Step 5: Live smoke** (manual, needs a login). Run `tf-architect run <small doc> --provider claude` and check:
  - the preflight passes;
  - a `~/.claude/projects` entry for the temp cwd does not exist afterwards;
  - `AGENTS.md` is written in the output.
- [ ] **Step 6:** Commit `feat(cli): provider selection, custom endpoint confirmation, security pop-up`.
