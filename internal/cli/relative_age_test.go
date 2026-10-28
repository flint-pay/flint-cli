package cli

import (
	"strings"
	"testing"
	"time"
)

func TestRelativeAgeDescribesPastAndFuture(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s ago"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
		{-30 * time.Second, "in 30s"},
		{-8*24*time.Hour - time.Hour, "in 8d"},
	} {
		if got := relativeAge(tc.d); got != tc.want {
			t.Errorf("relativeAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestHumanTimestampInTheFutureSaysIn(t *testing.T) {
	now := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	got := formatHumanScalar("2026-11-05T23:59:59Z", now)
	if !strings.HasSuffix(got, "(in 8d)") {
		t.Fatalf("formatHumanScalar(future evidence deadline) = %q, want a suffix of (in 8d)", got)
	}
}
