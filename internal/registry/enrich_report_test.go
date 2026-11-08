package registry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// defaultLevelLog returns a context whose logger keeps only what the shipped
// verbosity shows (V(0)..V(2), logging.DEFAULT), and the lines it kept.
func defaultLevelLog() (context.Context, *[]string) {
	var lines []string
	logger := funcr.New(func(prefix, args string) {
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 2})
	return log.IntoContext(context.Background(), logger), &lines
}

func mentions(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// TestUnusableTriggerMetadataIsVisibleAtDefaultVerbosity: a trigger whose
// metadata ParseMeta rejects is skipped by every engine, so the workload is held
// where it is. That used to be logged at DEBUG only, which the shipped
// verbosity hides. It must be said at the default level, once per change.
func TestUnusableTriggerMetadataIsVisibleAtDefaultVerbosity(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	// modelId, not modelID: the one-character typo the review found invisible.
	reg.Observe(testNamespace, "chat-so", map[string]string{"modelId": testModel})
	e, _ := newTestEnricher(t, &now, reg, scaledObject("chat-so", testTarget, nil, nil))

	ctx, lines := defaultLevelLog()
	e.Refresh(ctx)
	if got := mentions(*lines, "chat-so"); got != 1 {
		t.Fatalf("want one default-level line naming the ScaledObject, got %d: %v", got, *lines)
	}
	if mentions(*lines, ModelIDKey) == 0 {
		t.Errorf("the line must carry ParseMeta's reason (names %q): %v", ModelIDKey, *lines)
	}

	now = now.Add(2 * testTargetMaxAge)
	e.Refresh(ctx)
	if got := mentions(*lines, "chat-so"); got != 1 {
		t.Errorf("an unchanged problem must not be reported again, got %d lines", got)
	}
}

func TestUnusableTriggerMetadataRaisesOneWarningEvent(t *testing.T) {
	now := time.Now()
	reg := newTestRegistry(&now)
	reg.Observe(testNamespace, "chat-so", map[string]string{"modelId": testModel})
	e, _ := newTestEnricher(t, &now, reg, scaledObject("chat-so", testTarget, nil, nil))
	rec := record.NewFakeRecorder(10)
	e.Recorder = rec

	ctx := context.Background()
	e.Refresh(ctx)
	now = now.Add(2 * testTargetMaxAge)
	e.Refresh(ctx)

	if got := len(rec.Events); got != 1 {
		t.Fatalf("want exactly one event for a standing problem, got %d", got)
	}
	if ev := <-rec.Events; !strings.HasPrefix(ev, "Warning InvalidTriggerMetadata") {
		t.Errorf("event = %q", ev)
	}

	// Fixed: said once at the default level, no further event.
	reg.Observe(testNamespace, "chat-so", map[string]string{ModelIDKey: testModel})
	logCtx, lines := defaultLevelLog()
	e.Refresh(logCtx)
	if mentions(*lines, "usable again") != 1 {
		t.Errorf("the recovery must be reported once: %v", *lines)
	}
	if len(rec.Events) != 0 {
		t.Errorf("a recovered trigger raised another event")
	}

	// Broken again: reported again.
	reg.Observe(testNamespace, "chat-so", map[string]string{"modelId": testModel})
	e.Refresh(ctx)
	if len(rec.Events) != 1 {
		t.Errorf("a recurrence must be reported, got %d events", len(rec.Events))
	}
}
