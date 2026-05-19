package masking

import "regexp"

type maskRule struct {
	re          *regexp.Regexp
	category    string
	format      string
	valueGroup  int
	prefixGroup int
	suffixGroup int
}

var patterns = []maskRule{
	{
		re:       regexp.MustCompile(`arn:aws:[a-z0-9\-]+:[^:\s"']*:[^:\s"']*:[^\s"',]+`),
		category: "RESOURCE",
		format:   "arn:aws:%s",
	},
	{
		re:       regexp.MustCompile(`\b\d{12}\b`),
		category: "ACCOUNT",
		format:   "%s",
	},
	{
		re:       regexp.MustCompile(`\b(?:i|sg|vpc|subnet|rtb|igw|nat|ami|vol|snap|eni|acl|lb|tg)-[0-9a-f]{8,17}\b`),
		category: "RESOURCE_ID",
		format:   "%s",
	},
	{
		re:       regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})/\d{1,2}\b`),
		category: "CIDR_PRIVATE",
		format:   "%s",
	},
	{
		re:       regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}/\d{1,2}\b`),
		category: "CIDR_PUBLIC",
		format:   "%s",
	},
	{
		re:       regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})\b`),
		category: "IP_PRIVATE",
		format:   "%s",
	},
	{
		re:       regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`),
		category: "IP_PUBLIC",
		format:   "%s",
	},
	{
		re:       regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
		category: "EMAIL",
		format:   "%s",
	},
	{
		re:       regexp.MustCompile(`\b[a-z0-9][a-z0-9\-]*\.[a-z0-9][a-z0-9\-]*\.[a-z]{2,}\b`),
		category: "HOST",
		format:   "%s",
	},
	{
		re:          regexp.MustCompile(`(?i)(bucket(?:_name)?\s*[=:]\s*["'])([^"']+)(["'])`),
		category:    "BUCKET",
		format:      "%s",
		valueGroup:  2,
		prefixGroup: 1,
		suffixGroup: 3,
	},
	{
		re:          regexp.MustCompile(`(?i)((?:owner|Name|team|project|environment|env|application)\s*[=:]\s*["'])([^"']+)(["'])`),
		category:    "TAG_VALUE",
		format:      "%s",
		valueGroup:  2,
		prefixGroup: 1,
		suffixGroup: 3,
	},
}
