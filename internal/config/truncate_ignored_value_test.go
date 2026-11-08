package config

import (
	"strings"
	"testing"
)

// TruncateIgnoredValue keeps a short value as is and cuts a long one on a
// character boundary, saying how much it cut.
func TestTruncateIgnoredValue(t *testing.T) {
	if got := TruncateIgnoredValue("5 minutes"); got != "5 minutes" {
		t.Errorf("short value changed to %q", got)
	}
	exact := strings.Repeat("x", MaxIgnoredValueLen)
	if got := TruncateIgnoredValue(exact); got != exact {
		t.Errorf("a value of exactly %d bytes was cut: %q", MaxIgnoredValueLen, got)
	}
	multi := strings.Repeat("é", 100) // 2 bytes each
	got := TruncateIgnoredValue(multi)
	if !strings.HasPrefix(got, strings.Repeat("é", MaxIgnoredValueLen/2)+"...(") ||
		!strings.HasSuffix(got, "(136 more bytes)") {
		t.Errorf("multi-byte value cut badly: %q", got)
	}
	// An odd cut point falls back to the previous character boundary.
	odd := "a" + multi
	if got := TruncateIgnoredValue(odd); !strings.HasPrefix(got, "a"+strings.Repeat("é", 31)+"...(") {
		t.Errorf("odd cut point split a character: %q", got)
	}
}
