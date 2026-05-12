package ingestion

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ConversionResult struct {
	Markdown        string
	EstimatedTokens int
	SourceFormat    string
	WasConverted    bool
}

// Convert accepts any supported path and returns clean Markdown.
// Text extraction is always preferred over vision/OCR.
func Convert(path string) (*ConversionResult, error) {
	ext := strings.ToLower(filepath.Ext(path))

	switch ext {
	case ".md", ".txt", ".rst":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		md := string(data)
		return &ConversionResult{
			Markdown:        md,
			EstimatedTokens: estimateTokens(md),
			SourceFormat:    ext,
			WasConverted:    false,
		}, nil

	case ".pdf":
		return convertPDF(path)

	case ".docx", ".doc":
		return convertDOCX(path)

	default:
		return nil, fmt.Errorf("unsupported format: %s", ext)
	}
}

// convertPDF uses pdftotext (poppler) for native text extraction.
// Falls back to tesseract OCR if the PDF is image-only (scanned).
func convertPDF(path string) (*ConversionResult, error) {
	var out bytes.Buffer
	cmd := exec.Command("pdftotext", "-layout", "-enc", "UTF-8", path, "-")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftotext failed (install poppler-utils): %w", err)
	}
	extracted := out.String()

	if isScannedPDF(extracted, path) {
		return convertPDFwithOCR(path)
	}

	md := cleanAndMarkdownify(extracted)
	return &ConversionResult{
		Markdown:        md,
		EstimatedTokens: estimateTokens(md),
		SourceFormat:    ".pdf",
		WasConverted:    true,
	}, nil
}

func isScannedPDF(text, path string) bool {
	pages := countPDFPages(path)
	if pages == 0 {
		return false
	}
	charsPerPage := len(strings.TrimSpace(text)) / pages
	return charsPerPage < 100
}

func countPDFPages(path string) int {
	var out bytes.Buffer
	cmd := exec.Command("pdfinfo", path)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "Pages:") {
			var n int
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, "Pages:"), "%d", &n); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

func convertPDFwithOCR(path string) (*ConversionResult, error) {
	tmpDir, err := os.MkdirTemp("", "tf-ocr-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.Command("pdftoppm", "-r", "300", "-png", path, filepath.Join(tmpDir, "page"))
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftoppm failed (install poppler-utils): %w", err)
	}

	var allText strings.Builder
	pages, _ := filepath.Glob(filepath.Join(tmpDir, "page-*.png"))
	for _, page := range pages {
		var ocrOut bytes.Buffer
		ocrCmd := exec.Command("tesseract", page, "stdout", "-l", "spa+eng", "--psm", "1")
		ocrCmd.Stdout = &ocrOut
		if err := ocrCmd.Run(); err != nil {
			continue
		}
		allText.WriteString(ocrOut.String())
		allText.WriteString("\n\n")
	}

	md := cleanAndMarkdownify(allText.String())
	return &ConversionResult{
		Markdown:        md,
		EstimatedTokens: estimateTokens(md),
		SourceFormat:    ".pdf (scanned→OCR)",
		WasConverted:    true,
	}, nil
}

func convertDOCX(path string) (*ConversionResult, error) {
	if _, err := exec.LookPath("pandoc"); err == nil {
		return convertDOCXwithPandoc(path)
	}
	return convertDOCXwithPython(path)
}

func convertDOCXwithPandoc(path string) (*ConversionResult, error) {
	var out bytes.Buffer
	cmd := exec.Command("pandoc", path, "-t", "markdown_strict", "--wrap=none")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pandoc failed: %w", err)
	}
	md := out.String()
	return &ConversionResult{
		Markdown:        md,
		EstimatedTokens: estimateTokens(md),
		SourceFormat:    ".docx (pandoc)",
		WasConverted:    true,
	}, nil
}

func convertDOCXwithPython(path string) (*ConversionResult, error) {
	pyScript := `import sys, docx, re
def docx_to_md(path):
    doc = docx.Document(path)
    lines = []
    for p in doc.paragraphs:
        style = p.style.name.lower()
        text = p.text.strip()
        if not text:
            lines.append("")
            continue
        if "heading 1" in style:
            lines.append(f"# {text}")
        elif "heading 2" in style:
            lines.append(f"## {text}")
        elif "heading 3" in style:
            lines.append(f"### {text}")
        elif "list" in style:
            lines.append(f"- {text}")
        else:
            lines.append(text)
    for table in doc.tables:
        rows = []
        for i, row in enumerate(table.rows):
            cells = [c.text.strip().replace("\n", " ") for c in row.cells]
            rows.append("| " + " | ".join(cells) + " |")
            if i == 0:
                rows.append("| " + " | ".join(["---"] * len(cells)) + " |")
        lines.extend(rows)
        lines.append("")
    return "\n".join(lines)
print(docx_to_md(sys.argv[1]))`

	var out, stderr bytes.Buffer
	cmd := exec.Command("python3", "-c", pyScript, path)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("python-docx failed (pip install python-docx): %w\n%s", err, stderr.String())
	}
	md := out.String()
	return &ConversionResult{
		Markdown:        md,
		EstimatedTokens: estimateTokens(md),
		SourceFormat:    ".docx (python-docx)",
		WasConverted:    true,
	}, nil
}

// cleanAndMarkdownify removes repeating headers/footers and normalises PDF text.
func cleanAndMarkdownify(text string) string {
	lines := strings.Split(text, "\n")

	freq := make(map[string]int)
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if len(trimmed) > 5 && len(trimmed) < 100 {
			freq[trimmed]++
		}
	}

	// Threshold: a line must appear at least 3 times OR in >20% of pages to be
	// considered a header/footer. Minimum of 3 prevents removing unique repeated content.
	pageCount := strings.Count(text, "\f") + 1
	threshold := pageCount / 5
	if threshold < 3 {
		threshold = 3
	}

	var cleaned []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "\f" {
			continue
		}
		// Collapse consecutive blank lines but preserve single blank lines
		if trimmed == "" && len(cleaned) > 0 && cleaned[len(cleaned)-1] == "" {
			continue
		}
		if freq[trimmed] >= threshold {
			continue
		}
		// Keep original line (not trimmed) to preserve indentation in code blocks
		cleaned = append(cleaned, line)
	}

	return strings.Join(cleaned, "\n")
}

// estimateTokens approximates token count (~4 chars/token for technical EN/ES text).
func estimateTokens(text string) int {
	return len(text) / 4
}
