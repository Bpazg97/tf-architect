package validation_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tf-architect/internal/validation"
)

func TestGoldenRulesAuditPasses(t *testing.T) {
	dir := t.TempDir()
	content := `
terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
  backend "s3" {
    bucket = "my-tfstate"
    key    = "terraform.tfstate"
    region = "eu-west-1"
  }
}

provider "aws" {
  default_tags { tags = var.tags }
}

variable "tags" { type = map(string); default = {} }
`
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0644)

	suite := validation.New(dir)
	result := suite.RunGoldenRulesAudit()

	if !result.Passed {
		t.Errorf("expected golden rules to pass, got errors: %v", result.Errors)
	}
}

func TestGoldenRulesAuditDetectsMissingBackend(t *testing.T) {
	dir := t.TempDir()
	content := `
terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}
provider "aws" {
  default_tags { tags = {} }
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0644)

	suite := validation.New(dir)
	result := suite.RunGoldenRulesAudit()

	if result.Passed {
		t.Error("expected golden rules to fail due to missing backend")
	}

	found := false
	for _, e := range result.Errors {
		if len(e) > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected at least one error for missing backend")
	}
}

func TestGoldenRulesAuditDetectsOpenIngress(t *testing.T) {
	dir := t.TempDir()
	content := `
resource "aws_security_group" "bad" {
  ingress {
    cidr_blocks = ["0.0.0.0/0"]
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
  }
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0644)

	suite := validation.New(dir)
	result := suite.RunGoldenRulesAudit()

	if result.Passed {
		t.Error("expected golden rules to fail due to open ingress")
	}
}

func TestGoldenRulesAuditAllowsOpenEgress(t *testing.T) {
	dir := t.TempDir()
	content := `
resource "aws_security_group" "ok" {
  ingress {
    cidr_blocks = ["10.0.0.0/16"]
  }
  egress {
    cidr_blocks = ["0.0.0.0/0"]
  }
}
resource "aws_security_group_rule" "out" {
  type        = "egress"
  cidr_blocks = ["0.0.0.0/0"]
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0644)
	for _, e := range validation.New(dir).RunGoldenRulesAudit().Errors {
		if strings.Contains(e, "ingress") {
			t.Errorf("egress 0.0.0.0/0 reported as open ingress: %s", e)
		}
	}
}

func TestGoldenRulesAuditDetectsOpenIngressRuleResources(t *testing.T) {
	cases := map[string]string{
		"sg_rule": `resource "aws_security_group_rule" "in" {
  cidr_blocks = ["0.0.0.0/0"]
  type        = "ingress"
}`,
		"vpc_ingress_rule": `resource "aws_vpc_security_group_ingress_rule" "in" {
  cidr_ipv4 = "0.0.0.0/0"
}`,
	}
	for name, content := range cases {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0644)
		found := false
		for _, e := range validation.New(dir).RunGoldenRulesAudit().Errors {
			found = found || strings.Contains(e, "ingress")
		}
		if !found {
			t.Errorf("%s: open ingress not detected", name)
		}
	}
}

func TestGoldenRulesAuditIgnoresTerraformCache(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`terraform {
  required_version = ">= 1.5"
  required_providers {}
  backend "s3" {}
}
provider "aws" {
  default_tags {}
}`), 0644)
	cache := filepath.Join(dir, ".terraform", "modules", "x")
	_ = os.MkdirAll(cache, 0755)
	_ = os.WriteFile(filepath.Join(cache, "main.tf"), []byte(`resource "aws_security_group" "x" {
  ingress {
    cidr_blocks = ["0.0.0.0/0"]
  }
}`), 0644)
	if r := validation.New(dir).RunGoldenRulesAudit(); !r.Passed {
		t.Errorf("third-party module code in .terraform audited: %v", r.Errors)
	}
}

func TestModuleSourcesAndSkipTerraform(t *testing.T) {
	dir := t.TempDir()
	tf := "module \"a\" {\n  source = \"git::https://git.corp.example.com/a.git\"\n}\nmodule \"b\" {\n  source  = \"./modules/b\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0644); err != nil {
		t.Fatal(err)
	}
	got := validation.ModuleSources(dir)
	if strings.Join(got, ",") != "git::https://git.corp.example.com/a.git,./modules/b" {
		t.Errorf("ModuleSources = %v", got)
	}
	s := validation.New(dir)
	s.SkipTerraform = "not fetched"
	results, _ := s.Run()
	if len(results) == 0 || results[0].Stage != "terraform init/validate" || !results[0].Skipped || results[0].Output != "not fetched" {
		t.Errorf("terraform not skipped: %+v", results)
	}
	if last := results[len(results)-1]; last.Stage != "golden-rules-audit" {
		t.Errorf("later stages must still run, last = %s", last.Stage)
	}
}
