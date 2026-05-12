package validation_test

import (
	"os"
	"path/filepath"
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
