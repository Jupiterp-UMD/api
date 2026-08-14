package main

import (
	"container/list"
	"net/http"
	"sync"
	"time"
)

// Entries held per cache.
//
// This was 124, sized for a key space of department prefixes and a handful of
// course codes. Professor pages change that shape entirely: a per-slug key for
// every professor, plus a per-professor grade summary and a per-professor term
// series. At 124 entries that space thrashes to a near-zero hit rate and every
// request reaches Supabase, which is the opposite of what the cache is for.
const defaultCacheCapacity = 4096

// Course search runs on every page load and is the hottest path in the API.
// It gets its own cache so that a burst of professor-page traffic - which has
// a much larger key space - cannot evict it.
//
// The caches are per Cloud Run instance and there is no cross-instance
// invalidation; that is fine for data whose TTL is measured in hours, and is
// the reason review reads will need a much shorter TTL when they arrive.
const courseCacheCapacity = 2048

type cachedPayload struct {
	status int
	header http.Header
	body   []byte
}

type cacheEntry struct {
	key       string
	payload   *cachedPayload
	expiresAt time.Time
}

type LRUCache struct {
	capacity int
	mu       sync.Mutex
	items    map[string]*list.Element
	order    *list.List
}

func NewLRUCache(capacity int) *LRUCache {
	if capacity <= 0 {
		return nil
	}
	return &LRUCache{
		capacity: capacity,
		items:    make(map[string]*list.Element, capacity),
		order:    list.New(),
	}
}

func (c *LRUCache) Get(key string) (*cachedPayload, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		return nil, false
	}
	entry := elem.Value.(*cacheEntry)
	if time.Now().After(entry.expiresAt) {
		c.removeElement(elem)
		return nil, false
	}
	c.order.MoveToFront(elem)
	return entry.payload, true
}

func (c *LRUCache) Set(key string, payload *cachedPayload, ttl time.Duration) {
	if c == nil || ttl <= 0 || payload == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*cacheEntry)
		entry.payload = payload
		entry.expiresAt = time.Now().Add(ttl)
		c.order.MoveToFront(elem)
		return
	}
	entry := &cacheEntry{
		key:       key,
		payload:   payload,
		expiresAt: time.Now().Add(ttl),
	}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
	if c.order.Len() > c.capacity {
		c.evictOldest()
	}
}

func (c *LRUCache) removeElement(elem *list.Element) {
	entry := elem.Value.(*cacheEntry)
	delete(c.items, entry.key)
	c.order.Remove(elem)
}

func (c *LRUCache) evictOldest() {
	elem := c.order.Back()
	if elem != nil {
		c.removeElement(elem)
	}
}

func filterHeadersForCaching(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, values := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Te", "Trailers", "Transfer-Encoding", "Upgrade":
			continue
		default:
			copied := append([]string(nil), values...)
			dst[k] = copied
		}
	}
	return dst
}
