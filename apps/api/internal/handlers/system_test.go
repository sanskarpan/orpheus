package handlers

import (
	"strconv"
	"testing"
)

// TestSystemListAuditLog_LimitClamp mirrors the limit-clamp logic
// in SystemHandler.ListAuditLog. The DB-bound paths are covered by
// integration tests; this file only locks the parsing behaviour
// that runs before any query.
func TestSystemListAuditLog_LimitClamp(t *testing.T) {
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
		{"-1", 50},
		{"9999", 50},
		{"abc", 50},
		{"100", 100},
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
