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
// "phoenix-api", "PHOENIX") that is not inside a known placeholder.
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
		matches := re.FindAllStringIndex(text, -1)
		if matches == nil {
			continue
		}
		spans := placeholderSpans(text, s.mm)
		var sb strings.Builder
		last := 0
		for _, m := range matches {
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

// UnmaskText replaces placeholders in text with their real values. Only
// occurrences that Mask would produce again from the real value are restored
// (see restorable); any other placeholder-shaped text is left as is, so that
// restoring and re-masking can never turn a placeholder into an unmasked
// value.
func UnmaskText(text string, mm MaskMap) string {
	// A real value may itself contain a placeholder (an ARN masked after its
	// account ID was): repeat until stable. Each pass resolves one level.
	for pass := 0; pass <= len(mm); pass++ {
		spans := placeholderSpans(text, mm)
		if len(spans) == 0 {
			break
		}
		var sb strings.Builder
		last := 0
		for _, sp := range spans {
			sb.WriteString(text[last:sp[0]])
			sb.WriteString(mm[text[sp[0]:sp[1]]])
			last = sp[1]
		}
		sb.WriteString(text[last:])
		text = sb.String()
	}
	return text
}

// UnresolvedPlaceholders returns, in order of first appearance, the
// placeholder-shaped tokens in text that UnmaskText leaves as they are:
// values the model invented, altered or glued to other characters, which
// would be written to disk verbatim. Each is reported with the letters and
// digits touching it ("BUCKET_001v2").
func UnresolvedPlaceholders(text string, mm MaskMap) []string {
	resolved := placeholderSpans(text, mm)
	var unknown []string
	seen := map[string]bool{}
	for _, m := range placeholderRe.FindAllStringIndex(text, -1) {
		if overlapsAny(resolved, m[0], m[1]) {
			continue
		}
		start, end := m[0], m[1]
		for isAlnumAt(text, start-1) {
			start--
		}
		for isAlnumAt(text, end) {
			end++
		}
		if tok := text[start:end]; !seen[tok] {
			seen[tok] = true
			unknown = append(unknown, tok)
		}
	}
	return unknown
}

// arnPrefix is the literal prefix of RESOURCE placeholders.
const arnPrefix = "arn:aws:"

// placeholderSpans returns, in order, the [start, end) spans of the
// occurrences of mm's placeholders in text that UnmaskText restores. They
// are also the spans Mask protects from re-masking: only these are genuine
// placeholders; other placeholder-shaped text is ordinary text whose
// sensitive values must still be masked.
func placeholderSpans(text string, mm MaskMap) [][]int {
	var spans [][]int
	for _, m := range placeholderRe.FindAllStringIndex(text, -1) {
		// A placeholder body ends with its digits, so an occurrence ends
		// where the token does; it may start anywhere inside it
		// ("ZEBRA_TEAM_001" holds TEAM_001). The leftmost is the longest.
		for start := m[0]; start < m[1]; start++ {
			if start >= len(arnPrefix) && text[start-len(arnPrefix):start] == arnPrefix {
				if s := start - len(arnPrefix); restorable(text, s, m[1], mm) {
					spans = append(spans, []int{s, m[1]})
					break
				}
			}
			if restorable(text, start, m[1], mm) {
				spans = append(spans, []int{start, m[1]})
				break
			}
		}
	}
	return spans
}

// restorable reports whether text[start:end] is a placeholder of mm that
// Mask would produce again from its real value. Known values are masked
// only where no letter or digit touches them (replaceBounded), so such a
// placeholder is restored only under the same condition. Custom values are
// masked wherever they appear, so their placeholders are restored anywhere
// except before a digit (CUSTOM_001 inside CUSTOM_0012).
func restorable(text string, start, end int, mm MaskMap) bool {
	ph := text[start:end]
	if _, ok := mm[ph]; !ok {
		return false
	}
	if cat, _ := parsePlaceholder(ph); cat == customCategory {
		return end >= len(text) || text[end] < '0' || text[end] > '9'
	}
	return !isAlnumAt(text, start-1) && !isAlnumAt(text, end)
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
		text = s.replaceBounded(text, real, s.realToPlaceholder[real])
	}
	return text
}

// replaceBounded replaces occurrences of from that are not part of a longer
// alphanumeric token ("dev" must not hit "device", "10.0.0.1" must not hit
// "10.0.0.15") and that do not fall inside a known placeholder.
func (s *maskState) replaceBounded(text, from, to string) string {
	if !strings.Contains(text, from) {
		return text
	}
	spans := placeholderSpans(text, s.mm)
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

// IsSensitive reports whether text holds a sensitive value: masking would
// change it, or it contains a real value of mm (stored or fully resolved).
// mm is not modified.
func IsSensitive(text string, mm MaskMap) bool {
	if masked, _, _ := Mask(text, mm); masked != text {
		return true
	}
	for _, real := range mm {
		if real == "" {
			continue
		}
		if strings.Contains(text, real) || strings.Contains(text, UnmaskText(real, mm)) {
			return true
		}
	}
	return false
}
