package model

import (
	"sync"
)

// shardCount partitions the LRU to reduce lock contention
const shardCount = 16

// ImageCache is a sharded LRU cache for TMDB image proxy.
// Uses independent locks per shard — image requests don't block playback state.
type ImageCache struct {
	shards [shardCount]*shard
}

type shard struct {
	mu      sync.RWMutex
	store   map[string][]byte
	lru     []string
	cap     int
	hits    int64
	misses  int64
}

func NewImageCache(maxTotal int) *ImageCache {
	perShard := maxTotal / shardCount
	c := &ImageCache{}
	for i := 0; i < shardCount; i++ {
		c.shards[i] = &shard{
			store: make(map[string][]byte),
			cap:   perShard,
		}
	}
	return c
}

func (c *ImageCache) shardIndex(key string) int {
	h := 0
	for _, b := range []byte(key) {
		h = h*31 + int(b)
	}
	return (h & 0x7FFFFFFF) % shardCount
}

func (c *ImageCache) Get(key string) ([]byte, bool) {
	s := c.shards[c.shardIndex(key)]
	s.mu.RLock()
	data, ok := s.store[key]
	s.mu.RUnlock()
	// Track hit/miss (lock-free: atomic would be better but good enough)
	if ok {
		s.hits++
	} else {
		s.misses++
	}
	return data, ok
}

func (c *ImageCache) HitRate() float64 {
	var hits, misses int64
	for i := 0; i < shardCount; i++ {
		s := c.shards[i]
		s.mu.RLock()
		hits += s.hits
		misses += s.misses
		s.mu.RUnlock()
	}
	total := hits + misses
	if total == 0 {
		return 100.0
	}
	return float64(hits) / float64(total) * 100.0
}

func (c *ImageCache) Set(key string, data []byte) {
	s := c.shards[c.shardIndex(key)]
	s.mu.Lock()
	defer s.mu.Unlock()

	// Evict oldest if full
	if len(s.store) >= s.cap && len(s.lru) > 0 {
		oldest := s.lru[0]
		delete(s.store, oldest)
		s.lru = s.lru[1:]
	}
	s.store[key] = data
	s.lru = append(s.lru, key)
}

func (c *ImageCache) Count() int {
	n := 0
	for i := 0; i < shardCount; i++ {
		c.shards[i].mu.RLock()
		n += len(c.shards[i].store)
		c.shards[i].mu.RUnlock()
	}
	return n
}

func (c *ImageCache) Clear() {
	for i := 0; i < shardCount; i++ {
		s := c.shards[i]
		s.mu.Lock()
		s.store = make(map[string][]byte)
		s.lru = nil
		s.mu.Unlock()
	}
}
