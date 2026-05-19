package masking

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MaskMap maps placeholder → real value.
// Persisted as mask.json alongside session.json.
type MaskMap map[string]string

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

// Unmask walks all .tf files in dir and replaces placeholders with real values in-place.
// If a file cannot be read or written, a warning is appended but processing continues.
func Unmask(dir string, mm MaskMap) error {
	var warnings []string
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".tf") {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: read: %v", path, readErr))
			return nil
		}
		content := string(data)
		for placeholder, real := range mm {
			content = strings.ReplaceAll(content, placeholder, real)
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
			if _, already := s.mm[match]; already {
				return match
			}
			return s.makePlaceholder(rule.category, rule.format, match)
		})
	}
	return rule.re.ReplaceAllStringFunc(text, func(match string) string {
		subs := rule.re.FindStringSubmatch(match)
		if len(subs) <= rule.valueGroup {
			return match
		}
		value := subs[rule.valueGroup]
		if _, already := s.mm[value]; already {
			return match
		}
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
