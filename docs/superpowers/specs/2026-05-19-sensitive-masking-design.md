# Sensitive Value Masking — Design Spec

**Date:** 2026-05-19  
**Status:** Approved  
**Feature:** Security masking layer between document ingestion and Claude API

---

## Problem

tf-architect sends raw architectural documents to Claude. These documents may contain sensitive infrastructure values (ARNs, account IDs, IPs, owner names) that must not leave the organisation's perimeter unmasked.

## Goal

Mask all sensitive values **before** any text reaches Claude. After generation, de-mask all produced `.tf` files so they contain real values. Claude only ever sees deterministic placeholders.

---

## Architecture

New package: `internal/masking`

### Exported API

```go
// Mask replaces sensitive values with placeholders.
// Returns masked text and the mapping needed to reverse it.
func Mask(text string) (string, MaskMap, error)

// Unmask walks all .tf files in dir and substitutes placeholders back to real values.
func Unmask(dir string, mm MaskMap) error

// MaskMap: placeholder → real value
type MaskMap map[string]string
```

### Storage

`MaskMap` persisted as `mask.json` in the session directory (alongside `session.json`).

`state.Session` gains one field:

```go
MaskMapPath string  // absolute path to mask.json; empty = masking disabled
```

---

## Data Flow

```
1.  ingestion.Convert(docPath)          → raw DocMarkdown
2.  masking.Mask(DocMarkdown)           → maskedMarkdown + MaskMap
3.  Write MaskMap → <sessionDir>/mask.json
4.  sess.DocMarkdown = maskedMarkdown   ← Claude receives only placeholders
5.  PhaseAnalyze  → Claude(masked doc)
6.  PhaseClarify  → Claude(masked Q&A)
7.  PhaseGenerate → Claude writes .tf with placeholders
8.  masking.Unmask(outputDir, MaskMap)  → real values restored in .tf files
9.  PhaseValidate → terraform fmt/validate on de-masked files
```

De-masking happens **after** `PhaseGenerate` completes and **before** `runValidation`. This ensures `terraform validate` operates on real values.

---

## Sensitive Patterns

Patterns applied in priority order (longest/most specific first to avoid partial matches).

| Category | Regex trigger | Placeholder format |
|---|---|---|
| AWS ARN | `arn:aws:[a-z0-9\-]+:[^:\s]*:[^:\s]*:[^\s"']+` | `arn:aws:RESOURCE_001` |
| AWS Account ID | `\b\d{12}\b` (standalone) | `ACCOUNT_001` |
| AWS Resource ID | `\b(i\|sg\|vpc\|subnet\|rtb\|igw\|nat\|ami\|vol\|snap)-[0-9a-f]{8,17}\b` | `RESOURCE_ID_001` |
| IPv4 private CIDR | `(10\|172\.1[6-9]\|172\.2\d\|172\.3[01]\|192\.168)\.\d+\.\d+/\d+` | `CIDR_PRIVATE_001` |
| IPv4 public CIDR | any other `x.x.x.x/n` | `CIDR_PUBLIC_001` |
| IPv4 private | `(10\|172\.1[6-9]\|172\.2\d\|172\.3[01]\|192\.168)\.\d+\.\d+` | `IP_PRIVATE_001` |
| IPv4 public | remaining `x.x.x.x` | `IP_PUBLIC_001` |
| FQDN / hostname | `[a-z0-9\-]+\.[a-z0-9\-]+\.[a-z]{2,}` (min 2 dots) | `HOST_001` |
| S3 bucket name | value of `bucket\s*=\s*"([^"]+)"` | `BUCKET_001` |
| Tag value | value in `(owner\|Name\|team\|project\|env)\s*=\s*"([^"]+)"` context | `TAG_VALUE_001` |
| Email | `[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}` | `EMAIL_001` |

**Not masked:**
- AWS region names (`eu-west-1`, `us-east-1`) — Claude needs these for resource generation
- Terraform variable references (`var.x`, `local.x`, `data.x`)
- Terraform resource type names (`aws_vpc`, `aws_instance`)

**Idempotent:** same real value always maps to same placeholder within a document. Counters are per-category (001, 002…).

---

## Resume / Doc-Changed Behaviour

On `ModeDocChanged`:
1. Load existing `mask.json`.
2. Mask new document text.
3. **Merge:** values already in the map reuse their existing placeholder. New values get new IDs continuing from the last counter.
4. Save merged `mask.json`.

---

## CLI Flag

```
--no-mask    Disable masking (dev/debug only). Logged as a warning.
```

---

## Error Handling

| Situation | Behaviour |
|---|---|
| `mask.json` write fails | Hard error — abort. Never send unmasked data to Claude. |
| `Unmask` fails on one `.tf` file | Log warning, continue. Partial de-mask is better than crash. |
| Claude invents a placeholder that has no entry in MaskMap | Leave as-is, log warning at end of session. |
| Pattern compile error | Caught at `init()` time — panics on startup (static patterns must be valid). |

---

## Testing

File: `internal/masking/masker_test.go`

| Test | Assertion |
|---|---|
| `TestMaskARN` | Single ARN masked to `arn:aws:RESOURCE_001` |
| `TestMaskIdempotent` | Same value appearing twice → same placeholder both times |
| `TestMaskMultiType` | Doc with ARN + IP + tag → all masked, no placeholder collision |
| `TestUnmask` | Round-trip: `Mask` then `Unmask` → original text restored |
| `TestMaskMerge` | `ModeDocChanged`: existing values reuse IDs, new values get next ID |
| `TestNoFalsePositives` | `eu-west-1`, `var.owner`, `aws_vpc` not masked |
| `TestMaskJsonPersist` | `MaskMap` serialises/deserialises correctly |
