package decision_test

import (
	"sync"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
)

// waitForWake fails the test if ch does not fire promptly. The wake-up is sent
// synchronously from Set, so a short bound is enough and keeps a regression
// (never signalling) from hanging the suite.
func waitForWake(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for wake-up: %s", msg)
	}
}

func TestSubscribeWakesOnSetForThatTarget(t *testing.T) {
	s := decision.NewStore()
	ch, cancel := s.Subscribe("chat", "sample")
	defer cancel()

	s.Set("chat", "sample", 3)
	waitForWake(t, ch, "Set on the subscribed target")

	if d, ok := s.Get("chat", "sample"); !ok || d.DesiredReplicas != 3 {
		t.Fatalf("Get after Set = (%+v, %v), want DesiredReplicas 3", d, ok)
	}
}

func TestSubscribeIgnoresOtherTargets(t *testing.T) {
	s := decision.NewStore()
	ch, cancel := s.Subscribe("chat", "sample")
	defer cancel()

	s.Set("chat", "other", 1)   // same namespace, different name
	s.Set("other", "sample", 1) // same name, different namespace

	select {
	case <-ch:
		t.Fatal("woke for a target the subscriber did not register for")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWakeUpsCoalesce(t *testing.T) {
	s := decision.NewStore()
	ch, cancel := s.Subscribe("chat", "sample")
	defer cancel()

	// Three writes with nobody reading: the buffered channel must absorb them
	// rather than blocking Set, and collapse to a single pending wake-up.
	for i := int32(1); i <= 3; i++ {
		s.Set("chat", "sample", i)
	}

	waitForWake(t, ch, "first of three coalesced writes")
	select {
	case <-ch:
		t.Fatal("expected the three writes to coalesce into one wake-up")
	case <-time.After(50 * time.Millisecond):
	}

	// The subscriber reads current state on wake, so it sees the LAST write.
	if d, _ := s.Get("chat", "sample"); d.DesiredReplicas != 3 {
		t.Fatalf("DesiredReplicas = %d, want the latest write (3)", d.DesiredReplicas)
	}
}

func TestCancelStopsWakeUps(t *testing.T) {
	s := decision.NewStore()
	ch, cancel := s.Subscribe("chat", "sample")
	cancel()

	s.Set("chat", "sample", 1)

	select {
	case <-ch:
		t.Fatal("woke after cancel")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	s := decision.NewStore()
	_, cancel := s.Subscribe("chat", "sample")
	cancel()
	cancel() // must not panic or disturb the store
	s.Set("chat", "sample", 1)
}

func TestSetDoesNotBlockOnASlowSubscriber(t *testing.T) {
	s := decision.NewStore()
	// Subscribe and never read. A Set that tried a blocking send would deadlock
	// the optimize loop, so this is the property that matters most.
	_, cancel := s.Subscribe("chat", "sample")
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := int32(0); i < 100; i++ {
			s.Set("chat", "sample", i)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Set blocked on a subscriber that never reads")
	}
}

func TestConcurrentSubscribeSetAndCancel(t *testing.T) {
	// Exercises the lock discipline under -race: subscribers churn while writes
	// land continuously.
	s := decision.NewStore()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.Set("chat", "sample", 1)
			}
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ch, cancel := s.Subscribe("chat", "sample")
				select {
				case <-ch:
				default:
				}
				cancel()
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestMultipleSubscribersAllWake(t *testing.T) {
	s := decision.NewStore()
	chA, cancelA := s.Subscribe("chat", "sample")
	defer cancelA()
	chB, cancelB := s.Subscribe("chat", "sample")
	defer cancelB()

	s.Set("chat", "sample", 2)

	waitForWake(t, chA, "subscriber A")
	waitForWake(t, chB, "subscriber B")
}
