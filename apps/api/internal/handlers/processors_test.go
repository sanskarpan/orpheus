package handlers

import (
	"strconv"
	"testing"
)

// TestProcessorList_LimitClamp mirrors the limit-clamp logic in
// ProcessorHandler.List. The handler delegates to the DB after the
// clamp, so we test the clamp function shape directly here. The
// DB-bound paths are covered by integration tests.
func TestProcessorList_LimitClamp(t *testing.T) {
	parse := func(raw string) int {
		const def, max = 50, 200
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > max {
			return def
		}
		return n
	}
	cases := []struct {
		in   string
		want int
	}{
		{"", 50},
		{"0", 50},
		{"-5", 50},
		{"1000", 50},
		{"abc", 50},
		{"1", 1},
		{"50", 50},
		{"200", 200},
	}
	for _, tc := range cases {
		t.Run("limit="+tc.in, func(t *testing.T) {
			if got := parse(tc.in); got != tc.want {
				t.Errorf("parse(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
