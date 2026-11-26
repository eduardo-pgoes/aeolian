package host

import (
	"regexp"
	"strings"
)

var secretShape = regexp.MustCompile(`\b(gh[pousr]_|github_pat_|lin_api_|lin_oauth_)[A-Za-z0-9_]+`)

func redact(s string, literals ...string) string {
	for _, l := range literals {
		if len(l) >= 8 {
			s = strings.ReplaceAll(s, l, "[redacted]")
		}
	}
	return secretShape.ReplaceAllString(s, "${1}[redacted]")
}
