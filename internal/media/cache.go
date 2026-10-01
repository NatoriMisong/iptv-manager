package media

import (
	"container/list"
	"net/http"
	"sync"
	"time"
)

type cachedSegment struct {
	key     string
	data    []byte
	header  http.Header
	status  int
	expires time.Time
}
type segmentCache struct {
	mu        sync.Mutex
	max, used int64
	lru       *list.List
	items     map[string]*list.Element
}

func newSegmentCache(max int64) *segmentCache {
	return &segmentCache{max: max, lru: list.New(), items: make(map[string]*list.Element)}
}
func (c *segmentCache) get(key string) (cachedSegment, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return cachedSegment{}, false
	}
	v := e.Value.(cachedSegment)
	if time.Now().After(v.expires) {
		c.remove(e)
		return cachedSegment{}, false
	}
	c.lru.MoveToFront(e)
	return v, true
}
func segmentCost(v cachedSegment) int64 {
	cost := int64(cap(v.data) + len(v.key) + 256)
	for k, values := range v.header {
		cost += int64(len(k) + 64)
		for _, value := range values {
			cost += int64(len(value) + 32)
		}
	}
	return cost
}
func (c *segmentCache) remove(e *list.Element) {
	v := e.Value.(cachedSegment)
	delete(c.items, v.key)
	c.used -= segmentCost(v)
	c.lru.Remove(e)
}
func (c *segmentCache) put(v cachedSegment) {
	if c.max <= 0 || segmentCost(v) > c.max || len(v.data) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[v.key]; ok {
		c.remove(e)
	}
	for e := c.lru.Back(); e != nil; {
		prev := e.Prev()
		if time.Now().After(e.Value.(cachedSegment).expires) {
			c.remove(e)
		}
		e = prev
	}
	for c.used+segmentCost(v) > c.max || len(c.items) >= 512 {
		c.remove(c.lru.Back())
	}
	v.expires = time.Now().Add(20 * time.Second)
	c.items[v.key] = c.lru.PushFront(v)
	c.used += segmentCost(v)
}
