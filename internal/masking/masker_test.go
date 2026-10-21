package masking_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tf-architect/internal/masking"
)

func TestMaskARN(t *testing.T) {
	input := `role_arn = "arn:aws:iam::123456789012:role/my-service-role"`
	masked, mm, err := masking.Mask(input, nil)
	if err != nil {
		t.Fatalf("Mask() error: %v", err)
	}
	if strings.Contains(masked, "my-service-role") {
		t.Error("role name leaked into masked output")
	}
	if len(mm) == 0 {
		t.Error("MaskMap should not be empty")
	}
	for ph, real := range mm {
		if strings.HasPrefix(real, "arn:aws:") && !strings.HasPrefix(ph, "arn:aws:") {
			t.Errorf("ARN real value %q got non-ARN placeholder %q", real, ph)
		}
	}
}

func TestMaskIdempotent(t *testing.T) {
	input := `server_a = "10.0.1.5"
server_b = "10.0.1.5"`
	masked, mm, err := masking.Mask(input, nil)
	if err != nil {
		t.Fatalf("Mask() error: %v", err)
	}
	if strings.Count(masked, "IP_PRIVATE_001") != 2 {
		t.Errorf("expected IP_PRIVATE_001 twice, got:\n%s", masked)
	}
	if len(mm) != 1 {
		t.Errorf("expected 1 MaskMap entry, got %d: %v", len(mm), mm)
	}
}

func TestMaskMultiType(t *testing.T) {
	input := `arn = "arn:aws:s3:::my-company-prod-logs"
ip  = "10.0.0.1"
owner = "acme-corp"`
	masked, mm, err := masking.Mask(input, nil)
	if err != nil {
		t.Fatalf("Mask() error: %v", err)
	}
	if strings.Contains(masked, "my-company-prod-logs") {
		t.Error("ARN resource not masked")
	}
	if strings.Contains(masked, "10.0.0.1") {
		t.Error("private IP not masked")
	}
	if strings.Contains(masked, "acme-corp") {
		t.Error("tag value not masked")
	}
	seen := make(map[string]bool)
	for ph := range mm {
		if seen[ph] {
			t.Errorf("placeholder collision: %q", ph)
		}
		seen[ph] = true
	}
}

func TestUnmaskText(t *testing.T) {
	content := `resource "aws_instance" "web" {
  tags = {
    Owner = "TAG_VALUE_001"
    Env   = "TAG_VALUE_002"
  }
  private_ip = "IP_PRIVATE_001"
}`
	mm := masking.MaskMap{
		"TAG_VALUE_001":  "tmobilitat",
		"TAG_VALUE_002":  "production",
		"IP_PRIVATE_001": "10.0.1.5",
	}
	got := masking.UnmaskText(content, mm)
	if !strings.Contains(got, "tmobilitat") || !strings.Contains(got, "10.0.1.5") {
		t.Errorf("values not restored:\n%s", got)
	}
	if strings.Contains(got, "TAG_VALUE_001") {
		t.Error("placeholder still present after unmask")
	}
}

func TestMaskMerge(t *testing.T) {
	_, mm1, err := masking.Mask(`ip = "10.0.1.5"`, nil)
	if err != nil {
		t.Fatalf("first Mask() error: %v", err)
	}

	masked2, mm2, err := masking.Mask(`ip  = "10.0.1.5"
ip2 = "10.0.1.6"`, mm1)
	if err != nil {
		t.Fatalf("second Mask() error: %v", err)
	}

	if !strings.Contains(masked2, "IP_PRIVATE_001") {
		t.Errorf("existing IP should keep IP_PRIVATE_001, got:\n%s", masked2)
	}
	if !strings.Contains(masked2, "IP_PRIVATE_002") {
		t.Errorf("new IP should get IP_PRIVATE_002, got:\n%s", masked2)
	}
	if len(mm2) != 2 {
		t.Errorf("expected 2 MaskMap entries, got %d: %v", len(mm2), mm2)
	}
}

func TestNoFalsePositives(t *testing.T) {
	input := `provider "aws" {
  region = "eu-west-1"
}
resource "aws_vpc" "main" {
  cidr_block = var.vpc_cidr
}
variable "owner" {
  default = var.owner
}
locals {
  name = local.project_name
}
module.rds.endpoint
module.vpc.id
local.common_tags.name
local.endpoints.db`
	masked, mm, err := masking.Mask(input, nil)
	if err != nil {
		t.Fatalf("Mask() error: %v", err)
	}
	if !strings.Contains(masked, "eu-west-1") {
		t.Error("AWS region eu-west-1 must NOT be masked")
	}
	if !strings.Contains(masked, "aws_vpc") {
		t.Error("Terraform resource type aws_vpc must NOT be masked")
	}
	if !strings.Contains(masked, "var.owner") {
		t.Error("var.owner must NOT be masked")
	}
	if !strings.Contains(masked, "var.vpc_cidr") {
		t.Error("var.vpc_cidr must NOT be masked")
	}
	if !strings.Contains(masked, "local.project_name") {
		t.Error("local.project_name must NOT be masked")
	}
	if !strings.Contains(masked, "module.rds.endpoint") {
		t.Error("Terraform module reference module.rds.endpoint must NOT be masked")
	}
	if !strings.Contains(masked, "module.vpc.id") {
		t.Error("Terraform module reference module.vpc.id must NOT be masked")
	}
	if !strings.Contains(masked, "local.common_tags.name") {
		t.Error("Terraform local reference local.common_tags.name must NOT be masked")
	}
	if !strings.Contains(masked, "local.endpoints.db") {
		t.Error("Terraform local reference local.endpoints.db must NOT be masked")
	}
	if len(mm) != 0 {
		t.Errorf("expected empty MaskMap, got %v", mm)
	}
}

func TestMaskFQDN4Label(t *testing.T) {
	input := `endpoint = "db.prod.internal.example.com"`
	masked, mm, err := masking.Mask(input, nil)
	if err != nil {
		t.Fatalf("Mask() error: %v", err)
	}
	if strings.Contains(masked, "db.prod.internal.example.com") {
		t.Error("4-label FQDN should be masked")
	}
	if len(mm) != 1 {
		t.Fatalf("expected 1 MaskMap entry, got %d: %v", len(mm), mm)
	}
	// Placeholder must be the whole FQDN, not a partial match
	for ph, real := range mm {
		if real != "db.prod.internal.example.com" {
			t.Errorf("expected real value %q, got %q", "db.prod.internal.example.com", real)
		}
		if strings.Contains(ph, ".") {
			t.Errorf("placeholder should not contain dots: %q", ph)
		}
	}
}

func TestMaskJsonPersist(t *testing.T) {
	mm := masking.MaskMap{
		"ACCOUNT_001":    "123456789012",
		"IP_PRIVATE_001": "10.0.1.5",
		"TAG_VALUE_001":  "tmobilitat",
	}
	path := filepath.Join(t.TempDir(), "mask.json")

	if err := masking.SaveMaskMap(path, mm); err != nil {
		t.Fatalf("SaveMaskMap() error: %v", err)
	}
	loaded, err := masking.LoadMaskMap(path)
	if err != nil {
		t.Fatalf("LoadMaskMap() error: %v", err)
	}
	for ph, want := range mm {
		if got := loaded[ph]; got != want {
			t.Errorf("loaded[%q] = %q, want %q", ph, got, want)
		}
	}
	info, _ := os.Stat(path)
	if info.Mode()&0o044 != 0 {
		t.Errorf("mask.json should not be group/world readable, mode: %v", info.Mode())
	}
}

func TestMaskUnmaskTextRoundTrip(t *testing.T) {
	content := "private_ip = \"10.0.1.5\"\naccount_id = \"123456789012\"\npolicy = \"arn:aws:s3:::acme-secret-bucket\"\n"
	masked, mm, err := masking.Mask(content, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, real := range []string{"10.0.1.5", "123456789012", "acme-secret-bucket"} {
		if strings.Contains(masked, real) {
			t.Errorf("%q still present after Mask:\n%s", real, masked)
		}
	}
	if got := masking.UnmaskText(masked, mm); got != content {
		t.Errorf("round-trip mismatch:\nwant: %s\ngot:  %s", content, got)
	}
}

func TestUnmaskTextPlaceholderPrefixCollision(t *testing.T) {
	mm := masking.MaskMap{
		"ACCOUNT_100":  "111111111111",
		"ACCOUNT_1000": "222222222222",
	}
	got := masking.UnmaskText("a = \"ACCOUNT_100\"\nb = \"ACCOUNT_1000\"", mm)
	want := "a = \"111111111111\"\nb = \"222222222222\""
	if got != want {
		t.Errorf("prefix collision corrupted output:\nwant: %s\ngot:  %s", want, got)
	}
}

func TestUnresolvedPlaceholders(t *testing.T) {
	mm := masking.MaskMap{
		"CIDR_PRIVATE_001":     "10.0.0.0/16",
		"ACCOUNT_100":          "111111111111",
		"arn:aws:RESOURCE_001": "arn:aws:iam::111111111111:role/x",
	}
	text := "a = \"CIDR_PRIVATE_001\"\nb = \"CIDR_PRIVATE_999\"\nc = \"ACCOUNT_1000\"\n" +
		"d = \"arn:aws:RESOURCE_001\"\ne = \"CIDR_PRIVATE_999\"\n"
	got := masking.UnresolvedPlaceholders(text, mm)
	want := []string{"CIDR_PRIVATE_999", "ACCOUNT_1000"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("UnresolvedPlaceholders = %v, want %v", got, want)
	}
	if got := masking.UnresolvedPlaceholders("no placeholders here", mm); len(got) != 0 {
		t.Errorf("expected none, got %v", got)
	}
}

func TestMaskKnownValueIsBounded(t *testing.T) {
	mm := masking.MaskMap{"TAG_VALUE_001": "dev", "IP_PRIVATE_001": "10.0.0.1"}
	in := `device_type = "x"
stage: dev
prefix = "my-dev-vpc"
other = "10.0.0.15"`
	masked, mm, _ := masking.Mask(in, mm)
	if !strings.Contains(masked, "device_type") {
		t.Errorf("substring of identifier corrupted:\n%s", masked)
	}
	if !strings.Contains(masked, "stage: TAG_VALUE_001") || !strings.Contains(masked, "my-TAG_VALUE_001-vpc") {
		t.Errorf("known value without pattern context not masked:\n%s", masked)
	}
	if strings.Contains(masked, "IP_PRIVATE_0015") {
		t.Errorf("longer IP corrupted by shorter known IP:\n%s", masked)
	}
	if got := masking.UnmaskText(masked, mm); got != in {
		t.Errorf("round trip left placeholders:\n%s", got)
	}
}

func TestMaskAlreadyMaskedIsStable(t *testing.T) {
	_, mm, _ := masking.Mask(`Name = "acme-web"`, nil)
	masked, _, _ := masking.Mask(`Name = "acme-web"`, mm)
	again, mm2, _ := masking.Mask(masked, mm)
	if again != masked {
		t.Errorf("re-masking changed text: %q -> %q", masked, again)
	}
	if len(mm2) != len(mm) {
		t.Errorf("re-masking added placeholder-to-placeholder entries: %v", mm2)
	}
}

func TestUnmaskNestedPlaceholders(t *testing.T) {
	_, mm, _ := masking.Mask("account 123456789012", nil)
	masked, mm, _ := masking.Mask(`role = "arn:aws:iam::123456789012:role/acme-admin"`, mm)
	if strings.Contains(masked, "acme-admin") || strings.Contains(masked, "123456789012") {
		t.Fatalf("ARN with known account not fully masked: %s", masked)
	}
	want := `role = "arn:aws:iam::123456789012:role/acme-admin"`
	for i := 0; i < 20; i++ { // map order varies between runs
		if got := masking.UnmaskText(masked, mm); got != want {
			t.Fatalf("nested unmask failed:\nwant %s\ngot  %s", want, got)
		}
	}
}

func TestAnywhereCIDRNotMasked(t *testing.T) {
	masked, _, _ := masking.Mask(`cidr_blocks = ["0.0.0.0/0"]`, nil)
	if !strings.Contains(masked, "0.0.0.0/0") {
		t.Errorf("0.0.0.0/0 must stay visible for ingress review: %s", masked)
	}
}

func TestInterpolationNotMaskedAsTag(t *testing.T) {
	in := `Name = "${var.project}-vpc"`
	masked, _, _ := masking.Mask(in, nil)
	if masked != in {
		t.Errorf("interpolation masked: %s", masked)
	}
}

func TestCustomValuesMaskedAnyCaseAndPosition(t *testing.T) {
	mm := masking.AddCustom(nil, []string{"Phoenix", "ab", "acme"})
	in := `Project Phoenix: phoenix-api, PHOENIXDB, tag = "TAG_VALUE_001", acme.`
	masked, mm, _ := masking.Mask(in, mm)
	for _, leak := range []string{"Phoenix", "phoenix", "PHOENIX", "acme"} {
		if strings.Contains(masked, leak) {
			t.Errorf("custom value %q leaked: %s", leak, masked)
		}
	}
	if !strings.Contains(masked, "TAG_VALUE_001") {
		t.Errorf("existing placeholder corrupted: %s", masked)
	}
	for _, real := range mm {
		if real == "ab" {
			t.Error("custom value shorter than MinCustomLen must be ignored")
		}
	}
	if got := masking.UnmaskText(masked, mm); got != in {
		t.Errorf("round trip mismatch:\nwant %s\ngot  %s", in, got)
	}
}
