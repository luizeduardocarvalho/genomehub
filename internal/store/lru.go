package store

import (
	"container/list"
	"sync"
)

// lruCache is an in-memory size-accounted LRU index over the segment store. It
// holds no segment bytes — only each key's size and recency — and decides which
// keys to evict when the store exceeds its byte cap. It is enabled only on a
// bounded cache node (origins, the source of truth, run unbounded).
type lruCache struct {
	mu       sync.Mutex
	maxBytes int64
	curBytes int64
	ll       *list.List // front = most recently used
	items    map[string]*list.Element
}

type lruEntry struct {
	key  string
	size int64
}

func newLRU(maxBytes int64) *lruCache {
	return &lruCache{maxBytes: maxBytes, ll: list.New(), items: make(map[string]*list.Element)}
}

// add marks key as most-recently-used and returns the keys that must be evicted
// to stay within the cap (never including key itself). If key is already known
// it is just moved to the front and its size is unchanged.
func (c *lruCache) add(key string, size int64) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.ll.MoveToFront(e)
		return nil
	}
	c.items[key] = c.ll.PushFront(&lruEntry{key: key, size: size})
	c.curBytes += size

	var evict []string
	for c.curBytes > c.maxBytes {
		back := c.ll.Back()
		if back == nil {
			break
		}
		ent := back.Value.(*lruEntry)
		if ent.key == key {
			break // a single entry larger than the cap: keep it, don't thrash
		}
		c.ll.Remove(back)
		delete(c.items, ent.key)
		c.curBytes -= ent.size
		evict = append(evict, ent.key)
	}
	return evict
}

// touch moves key to most-recently-used on read; no-op if absent.
func (c *lruCache) touch(key string) {
	c.mu.Lock()
	if e, ok := c.items[key]; ok {
		c.ll.MoveToFront(e)
	}
	c.mu.Unlock()
}

// remove drops key from the index (after an external Delete).
func (c *lruCache) remove(key string) {
	c.mu.Lock()
	if e, ok := c.items[key]; ok {
		c.ll.Remove(e)
		delete(c.items, key)
		c.curBytes -= e.Value.(*lruEntry).size
	}
	c.mu.Unlock()
}

func (c *lruCache) stats() (cur, max int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curBytes, c.maxBytes
}
