package pool

import (
	"fmt"
	"time"
)

// FormatTimestamp renders t as the canonical UTC RFC3339 string every pool
// artifact persists (§6.6: "All persisted timestamps are UTC RFC 3339"). It
// normalizes to UTC first, so a caller holding a local time still gets an
// unambiguous string.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// ParseTimestamp parses a UTC RFC3339 timestamp string, rejecting anything
// that isn't valid RFC3339 and anything not already expressed in UTC (a
// non-"Z"/zero-offset zone). A stored value is expected to already comply
// with §6.6; a mismatch usually means the writer is buggy, and loud failure
// here is safer than silently normalizing and hiding that.
func ParseTimestamp(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing timestamp %q: %w", s, err)
	}
	if _, offset := t.Zone(); offset != 0 {
		return time.Time{}, fmt.Errorf("timestamp %q is not UTC (zone offset %d seconds)", s, offset)
	}
	return t, nil
}
