package pool

import (
	"testing"
	"time"
)

func TestFormatTimestampNormalizesToUTC(t *testing.T) {
	loc := time.FixedZone("CEST", 2*60*60)
	local := time.Date(2026, 7, 30, 14, 0, 0, 0, loc)

	got := FormatTimestamp(local)
	want := "2026-07-30T12:00:00Z"
	if got != want {
		t.Errorf("FormatTimestamp() = %q, want %q", got, want)
	}
}

func TestParseTimestampRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	s := FormatTimestamp(now)
	got, err := ParseTimestamp(s)
	if err != nil {
		t.Fatalf("ParseTimestamp: %v", err)
	}
	if !got.Equal(now) {
		t.Errorf("ParseTimestamp() = %v, want %v", got, now)
	}
}

func TestParseTimestampRejectsUnparsable(t *testing.T) {
	if _, err := ParseTimestamp("not-a-timestamp"); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestParseTimestampRejectsNonUTC(t *testing.T) {
	if _, err := ParseTimestamp("2026-07-30T09:00:00+02:00"); err == nil {
		t.Fatal("expected an error for a non-UTC offset, got nil")
	}
}

func TestParseTimestampAcceptsZeroOffsetSpelling(t *testing.T) {
	// "+00:00" is numerically UTC even though it isn't the "Z" spelling;
	// ParseTimestamp keys off the zone offset, not the literal suffix.
	got, err := ParseTimestamp("2026-07-30T09:00:00+00:00")
	if err != nil {
		t.Fatalf("ParseTimestamp: %v", err)
	}
	want := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParseTimestamp() = %v, want %v", got, want)
	}
}
