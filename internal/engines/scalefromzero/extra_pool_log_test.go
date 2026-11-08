package scalefromzero

import (
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"

	poolutil "github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/pool"
)

// A model whose variants resolve to more than one InferencePool has its queue
// read from the first only, so demand behind the others wakes nothing. That
// used to pass in silence; it is logged when the set of ignored pools changes,
// not on every 100ms tick, and once more when it clears.
func TestAModelOnTwoPoolsIsLoggedOncePerChange(t *testing.T) {
	var lines []string
	logger := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	e := &Engine{}
	group := modelGroup{namespace: "ns", modelID: "m"}
	used := &poolutil.EndpointPool{Namespace: "ns", Name: "pool-a"}

	count := func(s string) int {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, s) {
				n++
			}
		}
		return n
	}

	for range 3 {
		e.noteExtraPool(logger, group, used, []string{"ns/pool-b"})
	}
	if n := count("more than one InferencePool"); n != 1 {
		t.Fatalf("three ticks on the same two pools logged %d times, want 1:\n%s", n, strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], `"used"="ns/pool-a"`) || !strings.Contains(lines[0], "ns/pool-b") {
		t.Fatalf("the line names neither pool: %s", lines[0])
	}

	e.noteExtraPool(logger, group, used, []string{"ns/pool-c", "ns/pool-b"})
	if n := count("more than one InferencePool"); n != 2 {
		t.Fatalf("a changed set of pools was not logged again: %d", n)
	}

	e.noteExtraPool(logger, group, used, nil)
	e.noteExtraPool(logger, group, used, nil)
	if n := count("resolves to one InferencePool again"); n != 1 {
		t.Fatalf("clearing logged %d times, want 1", n)
	}
}
