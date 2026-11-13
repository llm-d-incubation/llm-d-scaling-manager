package whatifapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The cache drops the least recently used entry: a get counts as a use.
func TestLRURecencyAndEviction(t *testing.T) {
	c := newLRU[int](2)
	c.add("a", 1)
	c.add("b", 2)
	_, _ = c.get("a") // a is now the most recent
	c.add("c", 3)     // so b goes
	_, okA := c.get("a")
	_, okB := c.get("b")
	_, okC := c.get("c")
	assert.True(t, okA, "a was used, and dropped")
	assert.False(t, okB, "b was the least used, and kept")
	assert.True(t, okC)
	assert.Equal(t, 2, c.len())
}

// add keeps an entry's first value; put replaces it.
func TestLRUAddKeepsPutReplaces(t *testing.T) {
	c := newLRU[int](2)
	assert.Equal(t, 1, c.add("a", 1))
	assert.Equal(t, 1, c.add("a", 9), "add replaced an immutable entry")
	c.put("a", 7)
	v, _ := c.get("a")
	assert.Equal(t, 7, v)
	c.put("b", 2)
	c.put("c", 3)
	assert.Equal(t, 2, c.len(), "put grew the cache past its size")
}
