.PHONY: build test lint install clean deps-install

BINARY := tf-architect

build:
	go build -o $(BINARY) ./cmd/

test:
	go test ./... -v -count=1

lint:
	go vet ./...

install: build
	cp $(BINARY) /usr/local/bin/$(BINARY)

clean:
	rm -f $(BINARY)

deps-check:
	@which terraform     || echo "MISSING: terraform"
	@which tflint        || echo "MISSING: tflint (optional)"
	@which checkov       || echo "MISSING: checkov (optional)"
	@which terraform-docs || echo "MISSING: terraform-docs (optional)"
	@which pdftotext     || echo "MISSING: pdftotext (apt install poppler-utils)"
	@which pandoc        || echo "MISSING: pandoc (optional, improves .docx quality)"

deps-install:
	@echo "Installing optional IaC validation tools..."
	@which tflint || curl -s https://raw.githubusercontent.com/terraform-linters/tflint/master/install_linux.sh | bash
	@which terraform-docs || (curl -Lo /tmp/terraform-docs.tar.gz \
		https://github.com/terraform-docs/terraform-docs/releases/download/v0.19.0/terraform-docs-v0.19.0-linux-amd64.tar.gz && \
		tar -xzf /tmp/terraform-docs.tar.gz -C /tmp && \
		chmod +x /tmp/terraform-docs && \
		mv /tmp/terraform-docs /usr/local/bin/)
	@which checkov || pip install checkov
	@echo "Done. Run 'make deps-check' to verify."
