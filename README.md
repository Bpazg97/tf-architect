# tf-architect
 
Generate and validate production-grade AWS Terraform IaC from architecture documents using an AI model of your choice (Claude Code login, Anthropic, OpenAI, Gemini or any OpenAI-compatible endpoint).

## What it does

Feed `tf-architect` a PDF, DOCX, Markdown, or text document describing your AWS architecture and it will:

1. **Ask for custom sensitive values** — optional comma-separated list (`value1,value2,value3`, default `null`) of names the patterns cannot know about
2. **Mask sensitive values** — ARNs, IPs, account IDs, hostnames, tag values, emails and your custom values are replaced with stable placeholders before any text reaches the AI provider
3. Analyse the masked document with the model
4. Ask clarifying questions if information is missing (answers are masked too)
5. Generate production-grade Terraform modules (with placeholders). The model only writes and reviews code: it has no tools, no shell, and never runs `terraform` or anything else
6. Run best-effort checks locally (`terraform init/validate`, `tflint`, `checkov`, golden-rules audit — each skipped if not installed) on a de-masked temporary copy, with one AI auto-fix pass
7. Produce architectural and `terraform-docs` documentation — also when issues remain
8. **Restore real values** — every text file in the output directory is de-masked once, after the last AI call

> **You are responsible for the result.** tf-architect generates a starting point. Review, validate and test the code before using it. Remaining check failures are listed at the end and make the command exit non-zero.

Sessions are persisted — interrupted runs resume from the last checkpoint.

## Requirements

**Required**

- Go 1.26+
- One provider (see [Providers](#providers)): either the Claude CLI (`claude` in `$PATH`, version 2.1.306 or newer, already logged in) or an API key for Anthropic, OpenAI, Gemini or an OpenAI-compatible endpoint

**Optional (validation)**

- [Terraform](https://developer.hashicorp.com/terraform/install) — `init`/`validate` are skipped without it

- `tflint` — HCL linting
- `checkov` — security/compliance scan
- `terraform-docs` — module documentation
- `pdftotext` (`poppler-utils`) — PDF extraction
- `pandoc` — improved DOCX extraction

Check what's installed:

```sh
make deps-check
```

Install optional tools:

```sh
make deps-install
```

## Install

```sh
make install          # builds and copies to /usr/local/bin/tf-architect
```

Or build locally:

```sh
make build            # output: ./tf-architect
```

## Usage

### Generate Terraform from a document

```sh
tf-architect run architecture.pdf
```

Output goes to `./architecture-tf/` by default.

```sh
# Custom output directory
tf-architect run architecture.pdf --output ./infra/terraform

# Force a full re-run (discard saved session)
tf-architect run architecture.pdf --force

# Use a custom agents prompt / golden rules file (replaces the built-in AGENTS.md)
tf-architect run architecture.pdf --agents-file ./my-rules.md

# Pick a provider and model
OPENAI_API_KEY=... tf-architect run architecture.pdf --provider openai --model <model>

# Disable sensitive value masking (debug only — never use in production)
tf-architect run architecture.pdf --no-mask
```

### Providers

| `--provider` | Transport | Credentials (environment only) | Model |
|---|---|---|---|
| `claude` (default) | `claude -p` subprocess | your Claude Code login | CLI default, or `--model` |
| `anthropic` | HTTPS Messages API | `ANTHROPIC_API_KEY` | `--model`, default the latest Sonnet |
| `openai` | HTTPS Chat Completions | `OPENAI_API_KEY` | `--model` required |
| `gemini` | HTTPS generateContent | `GEMINI_API_KEY` | `--model` required |
| `openai-compatible` | HTTPS Chat Completions at `--base-url` | `TFA_API_KEY` | `--model` required |

API keys are read only from the environment, never from flags, and are never logged or persisted.

`--base-url` overrides the endpoint. It must be HTTPS (plain `http` only for `localhost`, `127.0.0.1` and `[::1]`). Any non-local `--base-url` asks `Continue? [y/N]` (default No) before anything is sent; non-interactive runs stop unless you pass `--yes-custom-endpoint`.

**Claude login isolation.** The `claude` provider runs the CLI with `-p --output-format stream-json --verbose --safe-mode --no-session-persistence --restricted --strict-mcp-config --setting-sources "" --tools "" --permission-prompts none`, `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1` and `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, in a fresh empty temporary directory. Your hooks, settings, plugins and memories are never touched and never loaded. A preflight call runs before any project data is sent, and the CLI's init event is checked on every call: any tool, MCP server, user plugin, wrong working directory or CLI older than 2.1.306 aborts the run with a pop-up.

### No tools: file protocol

The model never gets tools. It answers with plain text blocks that tf-architect validates and writes itself:

```
===FILE: modules/vpc/main.tf===
...content...
===END FILE===
===DELETE: old.tf===
NO_CHANGES
```

Paths must be relative, without `..`, `.terraform`, `.git` or `.tf-architect` segments, and use one of `.tf .tfvars .md .json .tpl .yaml .yml .sh`; anything else is rejected. Generation runs in steps (plan, one call per module, whole-project review), each call stateless, with at most 400 000 bytes of project per prompt. Real values live on disk; masking happens at the boundary of every call.

### AGENTS.md

`AGENTS.md` is the single source of truth for the model and for any AI tool opened on the generated repo. The built-in one (golden rules, module layout, file protocol) is replaced by `--agents-file`. At the end of a run `<output>/AGENTS.md` is written with a generated Project section (architecture summary, decisions, module map, conventions). It contains real infrastructure values after de-masking, so treat it like the `.tf` files.

### Sensitive value masking

By default, tf-architect automatically masks sensitive infrastructure values before sending any document content to the AI provider:

| Category | Example | Placeholder |
|----------|---------|-------------|
| AWS ARN | `arn:aws:iam::123456789012:role/my-role` | `arn:aws:RESOURCE_001` |
| Account ID | `123456789012` | `ACCOUNT_001` |
| Resource ID | `i-0abc123def`, `vpc-0abc123`, `sg-0abc123` | `RESOURCE_ID_001` |
| Private CIDR | `10.0.0.0/16`, `192.168.1.0/24` | `CIDR_PRIVATE_001` |
| Private IP | `10.0.1.5` | `IP_PRIVATE_001` |
| Hostname / FQDN | `db.internal.company.com` | `HOST_001` |
| Tag value | `owner = "acme-corp"` | `TAG_VALUE_001` |
| Email | `ops@company.com` | `EMAIL_001` |

| Custom value | `Phoenix` (also `phoenix-api`, `PHOENIX`) | `CUSTOM_001` |

**Custom values:** at the start of `run` you are asked for extra sensitive values as `value1,value2,value3`. Press Enter (or type `null`) for none. They are masked wherever they appear, ignoring case. Values shorter than 3 characters are ignored, and values cannot contain commas. Non-interactive runs (stdin not a terminal) skip the question.

> **Disclaimer:** automatic masking only covers the patterns above. Anything else you do not list may be sent to the AI provider. The author of tf-architect accepts no responsibility for data exposed through omission or negligence.

Placeholders are stable — the same value always gets the same placeholder across the entire document. The mapping is stored in `.tf-architect/sessions/<hash>/mask.json` (mode `0600`).

**Security invariant:** nothing sensitive reaches the AI provider, whichever provider you use.

- Every prompt and the system prompt pass through the masker inside the LLM client — the single choke point for all providers, which fails closed (if masking or saving `mask.json` fails, nothing is sent).
- Every text file in the output directory is masked before the first AI call of each run (including files you edited by hand) and de-masked exactly once when the run ends, on success or error.
- Validators need real values, so they run on a de-masked temporary copy; their errors are masked before being stored or sent.

**Confinement:** the model has no tools at all and sees only the text tf-architect puts in the prompt, so the source document and `.tf-architect/` (which holds `mask.json`) are out of reach. `run` refuses an output directory that contains either of them.

**Cleanup:** the Claude CLI's temporary working directory is removed after every call, and so is any `~/.claude/projects` entry the CLI may have created for it (only ours; nothing else under `~/.claude` is touched).

**Security stops and exit codes:** every fail-closed abort (masking or `mask.json` errors, failed preflight, unsafe model paths, truncated or over-budget output, an unconfirmed custom endpoint, an unsafe output directory) shows a red pop-up on stderr with what happened, why it matters and how to fix it, ending with `Nothing sensitive was sent to the AI provider.`

| Exit code | Meaning |
|---|---|
| 0 | success |
| 1 | error or remaining check failures |
| 2 | security stop (pop-up shown) |

> **Validation and testing are yours.** The model cannot run anything. Local checks are best-effort, so you must validate, plan and test the generated code before use.

**Interrupts:** the first Ctrl-C lets the run finish cleanup and de-mask the files. A second Ctrl-C quits immediately; files then stay masked until the next `run` restores them.

### Validate a Terraform directory

Runs the full validation suite against any Terraform project — not just ones generated by tf-architect.

```sh
tf-architect validate ./architecture-tf
```

Checks (in order):

| # | Check | Required |
|---|-------|----------|
| 1 | `terraform init` | if installed |
| 2 | `terraform validate` | if installed |
| 3 | `tflint` | if installed |
| 4 | `checkov` | if installed |
| 5 | Golden-rules audit | always |

**Golden rules** enforce: S3 backend, pinned provider versions, `default_tags`, no `0.0.0.0/0` ingress, no wildcard IAM actions, no plaintext secrets, no hardcoded AWS account IDs.

### Session management

```sh
tf-architect status architecture.pdf   # show phase, checkpoint, last validation
tf-architect reset architecture.pdf    # delete session (next run starts fresh)
```

## Development

```sh
make test    # go test ./... -v -count=1
make lint    # go vet ./...
make clean   # remove built binary
```
