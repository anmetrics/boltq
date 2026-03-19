package cache

import (
	"fmt"
	"strconv"
	"strings"
)

// matchPattern matches a simple glob pattern against a string.
// Supports: "*" (all), "prefix*", "*suffix", "*contains*", exact match.
func matchPattern(pattern, s string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}

	startsWithStar := strings.HasPrefix(pattern, "*")
	endsWithStar := strings.HasSuffix(pattern, "*")

	switch {
	case startsWithStar && endsWithStar:
		// *contains*
		inner := pattern[1 : len(pattern)-1]
		return strings.Contains(s, inner)
	case endsWithStar:
		// prefix*
		prefix := pattern[:len(pattern)-1]
		return strings.HasPrefix(s, prefix)
	case startsWithStar:
		// *suffix
		suffix := pattern[1:]
		return strings.HasSuffix(s, suffix)
	default:
		// exact match
		return s == pattern
	}
}

// parseInt64 parses a byte slice as int64.
func parseInt64(b []byte) (int64, error) {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("value is not an integer: %s", s)
	}
	return v, nil
}

// formatInt64 formats an int64 as a byte slice.
func formatInt64(v int64) []byte {
	return []byte(strconv.FormatInt(v, 10))
}
