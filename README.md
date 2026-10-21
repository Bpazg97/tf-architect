# tf-architect
 
Generate and validate production-grade AWS Terraform IaC from architecture documents using Claude AI.

## What it does

Feed `tf-architect` a PDF, DOCX, Markdown, or text document describing your AWS architecture and it will:

1. **Ask for custom sensitive values** — optional comma-separated list (`value1,value2,value3`, default `null`) of names the patterns cannot know about
2. **Mask sensitive values** — ARNs, IPs, account IDs, hostnames, tag values, emails and your custom values are replaced with stable placeholders before any text reaches Claude
3. Analyse the masked document with Claude
4. Ask clarifying questions if information is missing (answers are masked too)
5. Generate production-grade Terraform modules (with placeholders). Claude only writes and reviews code: it has no shell and never runs `terraform` or anything else
6. Run best-effort checks locally (`terraform init/validate`, `tflint`, `checkov`, golden-rules audit — each skipped if not installed) on a de-masked temporary copy, with one AI auto-fix pass
7. Produce architectural and `terraform-docs` documentation — also when issues remain
8. **Restore real values** — every text file in the output directory is de-masked once, after the last AI call

> **You are responsible for the result.** tf-architect generates a starting point. Review, validate and test the code before using it. Remaining check failures are listed at the end and make the command exit non-zero.

Sessions are persisted — interrupted runs resume from the last checkpoint.

## Requirements

**Required**

- Go 1.26+
- Claude CLI (`claude` in `$PATH`) with `ANTHROPIC_API_KEY` set — it runs in `--bare` mode, which never reads `claude login` credentials

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

# Use a custom system prompt / golden rules file
tf-architect run architecture.pdf --system-file ./my-rules.md

# Disable sensitive value masking (debug only — never use in production)
tf-architect run architecture.pdf --no-mask
```

### Sensitive value masking

By default, tf-architect automatically masks sensitive infrastructure values before sending any document content to Claude:

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

> **Disclaimer:** automatic masking only covers the patterns above. Anything else you do not list may be sent to Claude. The author of tf-architect accepts no responsibility for data exposed through omission or negligence.

Placeholders are stable — the same value always gets the same placeholder across the entire document. The mapping is stored in `.tf-architect/sessions/<hash>/mask.json` (mode `0600`).

**Security invariant:** nothing sensitive reaches Claude.

- Every prompt and the system prompt pass through the masker inside the Claude client — a single choke point that fails closed (if masking or saving `mask.json` fails, nothing is sent).
- Every text file in the output directory is masked before the first AI call of each run (including files you edited by hand) and de-masked exactly once when the run ends, on success or error.
- Validators need real values, so they run on a de-masked temporary copy; their errors are masked before being stored or sent.

**Confinement:** Claude runs with `--restricted` and only the file tools (`Read, Write, Edit, Glob, Grep`), with the output directory as its working directory. It has no shell and cannot read outside that directory, so the source document and `.tf-architect/` (which holds `mask.json`) are out of reach. `run` refuses an output directory that contains either of them.

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
