package steadystate

import (
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
)

// A group over its quota is said once when it goes over, again when the
// overage changes, and once when it is back within the quota; a group that
// stays within it says nothing.
func TestShareReportOverQuotaOncePerChange(t *testing.T) {
	var lines []string
	logger := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	var st utilizationShareState
	for _, over := range []int{0, 3, 3, 1, 0, 0} {
		st.reportOverQuota(logger, "A100/ns", over, 7)
	}
	if len(lines) != 3 {
		t.Fatalf("logged %d lines, want 3 (over 3, over 1, back within):\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i, want := range []string{`"overQuotaGPUs"=3`, `"overQuotaGPUs"=1`, "back within its quota"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d is %s, want it to contain %s", i, lines[i], want)
		}
	}
}
