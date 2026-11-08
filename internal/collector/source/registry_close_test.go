package source

import (
	"context"
	"testing"
)

type closingSource struct {
	NoOpSource
	closed int
}

func (c *closingSource) Close() { c.closed++ }

var _ Closer = (*closingSource)(nil)

func TestRegistryClosesReplacedAndRemovedSources(t *testing.T) {
	r := NewSourceRegistry()
	first, second := &closingSource{}, &closingSource{}

	if err := r.Update("pool", first); err != nil {
		t.Fatal(err)
	}
	if err := r.Update("pool", first); err != nil {
		t.Fatal(err)
	}
	if first.closed != 0 {
		t.Fatalf("re-registering the same source closed it %d times", first.closed)
	}

	if err := r.Update("pool", second); err != nil {
		t.Fatal(err)
	}
	if first.closed != 1 {
		t.Fatalf("replaced source closed %d times, want 1", first.closed)
	}

	if err := r.Unregister("pool"); err != nil {
		t.Fatal(err)
	}
	if second.closed != 1 {
		t.Fatalf("removed source closed %d times, want 1", second.closed)
	}
	if err := r.Unregister("pool"); err != nil {
		t.Fatalf("second Unregister: %v", err)
	}
}

func TestCacheCloseIsIdempotentAndKeepsEntries(t *testing.T) {
	c := NewCache(context.Background(), 0, 1)
	key := BuildCacheKey("q", nil)
	c.Set(key, MetricResult{QueryName: "q"}, 1<<40)
	c.Close()
	c.Close()
	if _, ok := c.Get(key); !ok {
		t.Fatal("a closed cache lost its entry")
	}
}
