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

// placeholderRe matches any placeholder token produced by makePlaceholder.
var placeholderRe = regexp.MustCompile(`[A-Z]+(?:_[A-Z]+)*_\d{3,}`)

// Mask replaces sensitive values in text with stable placeholders.
// Values already in existing are replaced first (even without the context a
// pattern needs, e.g. a tag value quoted in a tool error), then the
// patterns run. Text that is already masked is left unchanged.
// Pass nil for a fresh run. Returns masked text and complete (merged) MaskMap.
func Mask(text string, existing MaskMap) (string, MaskMap, error) {
	s := newMaskState(existing)
	result := s.replaceCustom(text)
	result = s.replaceKnown(result)
	for i := range patterns {
		result = patterns[i].apply(result, s)
	}
	return result, s.mm, nil
}

// customCategory marks values the user declared sensitive. They are masked
// wherever they appear, ignoring case and word boundaries.
const customCategory = "CUSTOM"

// MinCustomLen is the shortest accepted custom value: shorter ones would
// mask fragments of almost every word.
const MinCustomLen = 3

// AddCustom returns mm extended with a placeholder for each user-declared
// sensitive value. Values shorter than MinCustomLen are ignored.
func AddCustom(mm MaskMap, values []string) MaskMap {
	s := newMaskState(mm)
	for _, v := range values {
		if len(v) >= MinCustomLen {
			s.makePlaceholder(customCategory, "%s", v)
		}
	}
	return s.mm
}

// replaceCustom masks every case variant of the custom values ("Phoenix",
// "phoenix-api", "PHOENIX") that is not inside an existing placeholder.
func (s *maskState) replaceCustom(text string) string {
	var customs []string
	for ph, real := range s.mm {
		if cat, _ := parsePlaceholder(ph); cat == customCategory {
			customs = append(customs, real)
		}
	}
	sort.Slice(customs, func(i, j int) bool { return len(customs[i]) > len(customs[j]) })
	for _, v := range customs {
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(v))
		spans := placeholderRe.FindAllStringIndex(text, -1)
		var sb strings.Builder
		last := 0
		for _, m := range re.FindAllStringIndex(text, -1) {
			if overlapsAny(spans, m[0], m[1]) {
				continue
			}
			sb.WriteString(text[last:m[0]])
			sb.WriteString(s.makePlaceholder(customCategory, "%s", text[m[0]:m[1]]))
			last = m[1]
		}
		sb.WriteString(text[last:])
		text = sb.String()
	}
	return text
}

// UnmaskText replaces placeholders in text with their real values.
func UnmaskText(text string, mm MaskMap) string {
	// Longer placeholders first: FOO_100 must not corrupt FOO_1000.
	pairs := make([][2]string, 0, len(mm))
	for ph, real := range mm {
		pairs = append(pairs, [2]string{ph, real})
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i][0]) > len(pairs[j][0]) })

	// A real value may itself contain a placeholder (an ARN masked after its
	// account ID was): repeat until stable. Each pass resolves one level.
	for i := 0; i <= len(pairs); i++ {
		next := text
		for _, p := range pairs {
			next = replacePlaceholder(next, p[0], p[1])
		}
		if next == text {
			break
		}
		text = next
	}
	return text
}

// UnresolvedPlaceholders returns, in order of first appearance, the
// placeholder-shaped tokens in text that mm cannot resolve: values the model
// invented or altered, which would be written to disk verbatim.
func UnresolvedPlaceholders(text string, mm MaskMap) []string {
	known := make(map[string]bool, len(mm))
	for ph := range mm {
		// arn:aws:RESOURCE_001 is matched by its RESOURCE_001 body.
		for _, body := range placeholderRe.FindAllString(ph, -1) {
			known[body] = true
		}
	}
	var unknown []string
	seen := map[string]bool{}
	for _, tok := range placeholderRe.FindAllString(text, -1) {
		if !known[tok] && !seen[tok] {
			seen[tok] = true
			unknown = append(unknown, tok)
		}
	}
	return unknown
}

// SaveMaskMap writes mm to path as JSON with 0600 permissions.
func SaveMaskMap(path string, mm MaskMap) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
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

// replaceKnown replaces every known real value in text with its placeholder.
// Longer values first: an account ID inside an ARN must not split the ARN.
func (s *maskState) replaceKnown(text string) string {
	reals := make([]string, 0, len(s.realToPlaceholder))
	for real := range s.realToPlaceholder {
		if real != "" {
			reals = append(reals, real)
		}
	}
	sort.Slice(reals, func(i, j int) bool { return len(reals[i]) > len(reals[j]) })
	for _, real := range reals {
		text = replaceBounded(text, real, s.realToPlaceholder[real])
	}
	return text
}

// replaceBounded replaces occurrences of from that are not part of a longer
// alphanumeric token ("dev" must not hit "device", "10.0.0.1" must not hit
// "10.0.0.15") and that do not fall inside an existing placeholder.
func replaceBounded(text, from, to string) string {
	if !strings.Contains(text, from) {
		return text
	}
	spans := placeholderRe.FindAllStringIndex(text, -1)
	var sb strings.Builder
	i := 0
	for {
		j := strings.Index(text[i:], from)
		if j < 0 {
			break
		}
		start, end := i+j, i+j+len(from)
		if !isAlnumAt(text, start-1) && !isAlnumAt(text, end) && !overlapsAny(spans, start, end) {
			sb.WriteString(text[i:start])
			sb.WriteString(to)
			i = end
		} else {
			sb.WriteString(text[i : start+1])
			i = start + 1
		}
	}
	sb.WriteString(text[i:])
	return sb.String()
}

// replacePlaceholder replaces ph unless it is followed by a digit
// (ACCOUNT_100 inside ACCOUNT_1000).
func replacePlaceholder(text, ph, real string) string {
	if !strings.Contains(text, ph) {
		return text
	}
	var sb strings.Builder
	i := 0
	for {
		j := strings.Index(text[i:], ph)
		if j < 0 {
			break
		}
		start, end := i+j, i+j+len(ph)
		sb.WriteString(text[i:start])
		if end < len(text) && text[end] >= '0' && text[end] <= '9' {
			sb.WriteString(ph)
		} else {
			sb.WriteString(real)
		}
		i = end
	}
	sb.WriteString(text[i:])
	return sb.String()
}

func isAlnumAt(text string, i int) bool {
	if i < 0 || i >= len(text) {
		return false
	}
	c := text[i]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func overlapsAny(spans [][]int, start, end int) bool {
	for _, sp := range spans {
		if start < sp[1] && end > sp[0] {
			return true
		}
	}
	return false
}

func (s *maskState) makePlaceholder(category, format, value string) string {
	if _, isPlaceholder := s.mm[value]; isPlaceholder {
		return value
	}
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
			for _, v := range rule.skipValues {
				if match == v {
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
		// Terraform interpolations are code, not data.
		if strings.Contains(value, "${") {
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
