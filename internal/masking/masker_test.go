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

func TestUnmask(t *testing.T) {
	dir := t.TempDir()
	content := `resource "aws_instance" "web" {
  tags = {
    Owner = "TAG_VALUE_001"
    Env   = "TAG_VALUE_002"
  }
  private_ip = "IP_PRIVATE_001"
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("IP_PRIVATE_001"), 0644); err != nil {
		t.Fatal(err)
	}

	mm := masking.MaskMap{
		"TAG_VALUE_001":  "tmobilitat",
		"TAG_VALUE_002":  "production",
		"IP_PRIVATE_001": "10.0.1.5",
	}
	if err := masking.Unmask(dir, mm); err != nil {
		t.Fatalf("Unmask() error: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, "main.tf"))
	if !strings.Contains(string(got), "tmobilitat") {
		t.Error("tmobilitat not restored in .tf file")
	}
	if !strings.Contains(string(got), "10.0.1.5") {
		t.Error("10.0.1.5 not restored in .tf file")
	}
	if strings.Contains(string(got), "TAG_VALUE_001") {
		t.Error("placeholder still present after unmask")
	}

	notf, _ := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if !strings.Contains(string(notf), "IP_PRIVATE_001") {
		t.Error("non-.tf file should not be modified by Unmask")
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
