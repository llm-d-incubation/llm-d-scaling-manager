package whatifapi

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// lru is a least-recently-used cache of at most size entries. Its entries are
// immutable once added, so a reader holds one without the lock.
type lru[V any] struct {
	mu    sync.Mutex
	size  int
	order *list.List
	items map[string]*list.Element
}

// lruItem is one entry: its key, for eviction, and its value.
type lruItem[V any] struct {
	key   string
	value V
}

// newLRU is an empty cache of at most size entries, 1 at least.
func newLRU[V any](size int) *lru[V] {
	return &lru[V]{size: max(size, 1), order: list.New(), items: map[string]*list.Element{}}
}

// put sets key to value, replacing what it held: for an entry that lapses
// (a review's answer), where add would keep the stale one.
func (c *lru[V]) put(key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.Value = lruItem[V]{key: key, value: value}
		c.order.MoveToFront(e)
		return
	}
	c.items[key] = c.order.PushFront(lruItem[V]{key: key, value: value})
	c.evict()
}

// evict drops the least used entries past size. The caller holds the lock.
func (c *lru[V]) evict() {
	for c.order.Len() > c.size {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(lruItem[V]).key)
	}
}

// len is how many entries the cache holds.
func (c *lru[V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// get is the value at key, marked as used.
func (c *lru[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		return e.Value.(lruItem[V]).value, true
	}
	var zero V
	return zero, false
}

// add sets key to value, evicting the least used entry past size. A key
// already present keeps its value: entries are immutable, and two callers
// that priced the same key priced the same value.
func (c *lru[V]) add(key string, value V) V {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		return e.Value.(lruItem[V]).value
	}
	c.items[key] = c.order.PushFront(lruItem[V]{key: key, value: value})
	c.evict()
	return value
}

// contentID is "sha256:" and the hash of v's JSON, prefixed by kind, so that
// an inputs id and a scenario id never collide.
func contentID(kind string, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Every value hashed here is a plain struct of the package's own
		// types; one that does not marshal is a bug, not a request.
		panic(err)
	}
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write(b)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// curveKey names a fleet's curve set: the inputs, every knob but the
// headroom, which moves only the cap marker, and whether TPOT is drawn.
func curveKey(inputsID string, k Knobs, tpot bool) string {
	k.Headroom = 0
	return contentID("curves", struct {
		InputsID string
		Knobs    Knobs
		TPOT     bool
	}{inputsID, k, tpot})
}
