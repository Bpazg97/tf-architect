package masking

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaskMap maps placeholder → real value.
// Persisted as mask.json alongside session.json.
type MaskMap map[string]string

// maskedSuffixes are the generated-file extensions that may contain
// placeholders (Terraform code, tfvars, and Claude-written docs).
var maskedSuffixes = []string{".tf", ".tfvars", ".md"}

// Mask replaces sensitive values in text with stable placeholders.
// Pass an existing MaskMap to reuse placeholders on resume/doc-changed runs.
// Pass nil for a fresh run. Returns masked text and complete (merged) MaskMap.
func Mask(text string, existing MaskMap) (string, MaskMap, error) {
	s := newMaskState(existing)
	result := text
	for i := range patterns {
		result = patterns[i].apply(result, s)
	}
	return result, s.mm, nil
}

// Unmask walks all masked-suffix files in dir and replaces placeholders with
// real values in-place. If a file cannot be read or written, a warning is
// appended but processing continues.
func Unmask(dir string, mm MaskMap) error {
	// Longer placeholders first: FOO_100 must not corrupt FOO_1000.
	pairs := sortedPairs(mm, func(ph, real string) (string, string) { return ph, real })
	return replaceInDir(dir, pairs)
}

// MaskDir is the inverse of Unmask: replaces known real values with their
// placeholders in all masked-suffix files. Used before handing existing
// (already de-masked) files to Claude for edits or auto-fix.
func MaskDir(dir string, mm MaskMap) error {
	// Longer values first: an account ID inside an ARN must not split the ARN.
	pairs := sortedPairs(mm, func(ph, real string) (string, string) { return real, ph })
	return replaceInDir(dir, pairs)
}

func sortedPairs(mm MaskMap, order func(ph, real string) (from, to string)) [][2]string {
	pairs := make([][2]string, 0, len(mm))
	for ph, real := range mm {
		from, to := order(ph, real)
		pairs = append(pairs, [2]string{from, to})
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i][0]) > len(pairs[j][0]) })
	return pairs
}

func hasMaskedSuffix(path string) bool {
	for _, suf := range maskedSuffixes {
		if strings.HasSuffix(path, suf) {
			return true
		}
	}
	return false
}

func replaceInDir(dir string, pairs [][2]string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	var warnings []string
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !hasMaskedSuffix(path) {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: read: %v", path, readErr))
			return nil
		}
		content := string(data)
		for _, p := range pairs {
			content = strings.ReplaceAll(content, p[0], p[1])
		}
		if writeErr := os.WriteFile(path, []byte(content), info.Mode()); writeErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: write: %v", path, writeErr))
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if len(warnings) > 0 {
		return fmt.Errorf("unmask partial: %s", strings.Join(warnings, "; "))
	}
	return nil
}

// SaveMaskMap writes mm to path as JSON with 0600 permissions.
func SaveMaskMap(path string, mm MaskMap) error {
	data, err := json.MarshalIndent(mm, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadMaskMap reads a MaskMap from a JSON file at path.
func LoadMaskMap(path string) (MaskMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mm MaskMap
	return mm, json.Unmarshal(data, &mm)
}

type maskState struct {
	realToPlaceholder map[string]string
	mm                MaskMap
	counters          map[string]int
}

func newMaskState(existing MaskMap) *maskState {
	s := &maskState{
		realToPlaceholder: make(map[string]string),
		mm:                make(MaskMap),
		counters:          make(map[string]int),
	}
	for ph, real := range existing {
		s.mm[ph] = real
		s.realToPlaceholder[real] = ph
		cat, idx := parsePlaceholder(ph)
		if idx > s.counters[cat] {
			s.counters[cat] = idx
		}
	}
	return s
}

func (s *maskState) makePlaceholder(category, format, value string) string {
	if ph, ok := s.realToPlaceholder[value]; ok {
		return ph
	}
	s.counters[category]++
	id := fmt.Sprintf("%s_%03d", category, s.counters[category])
	ph := fmt.Sprintf(format, id)
	s.mm[ph] = value
	s.realToPlaceholder[value] = ph
	return ph
}

var placeholderBodyRe = regexp.MustCompile(`([A-Z_]+)_(\d+)$`)

func parsePlaceholder(ph string) (string, int) {
	if i := strings.LastIndex(ph, ":"); i >= 0 {
		ph = ph[i+1:]
	}
	m := placeholderBodyRe.FindStringSubmatch(ph)
	if len(m) < 3 {
		return "", 0
	}
	idx, _ := strconv.Atoi(m[2])
	return m[1], idx
}

func (rule *maskRule) apply(text string, s *maskState) string {
	if rule.valueGroup == 0 {
		return rule.re.ReplaceAllStringFunc(text, func(match string) string {
			for _, pfx := range rule.skipPrefixes {
				if strings.HasPrefix(match, pfx) {
					return match
				}
			}
			return s.makePlaceholder(rule.category, rule.format, match)
		})
	}
	// context-aware rule: only the value subgroup is sensitive
	return rule.re.ReplaceAllStringFunc(text, func(match string) string {
		subs := rule.re.FindStringSubmatch(match)
		if len(subs) <= rule.valueGroup {
			return match
		}
		value := subs[rule.valueGroup]
		placeholder := s.makePlaceholder(rule.category, rule.format, value)
		prefix, suffix := "", ""
		if rule.prefixGroup > 0 && len(subs) > rule.prefixGroup {
			prefix = subs[rule.prefixGroup]
		}
		if rule.suffixGroup > 0 && len(subs) > rule.suffixGroup {
			suffix = subs[rule.suffixGroup]
		}
		return prefix + placeholder + suffix
	})
}
