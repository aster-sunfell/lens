package main

import (
	"context"
	"sync"
	"time"
)

type cacheEntry struct {
	evidence Evidence
	expires  time.Time
	usedAt   time.Time
}

type cacheFlight struct {
	done     chan struct{}
	evidence Evidence
	err      error
}

type EvidenceCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	entries    map[string]cacheEntry
	flights    map[string]*cacheFlight
}

func NewEvidenceCache(ttl time.Duration, maxEntries int) *EvidenceCache {
	return &EvidenceCache{
		ttl:        ttl,
		maxEntries: maxEntries,
		entries:    make(map[string]cacheEntry),
		flights:    make(map[string]*cacheFlight),
	}
}

func (c *EvidenceCache) GetOrCompute(ctx context.Context, key string, compute func(context.Context) (Evidence, error)) (Evidence, bool, error) {
	now := time.Now()
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok {
		if now.Before(entry.expires) {
			entry.usedAt = now
			c.entries[key] = entry
			c.mu.Unlock()
			return entry.evidence, true, nil
		}
		delete(c.entries, key)
	}
	if flight, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return Evidence{}, false, ctx.Err()
		case <-flight.done:
			return flight.evidence, false, flight.err
		}
	}

	flight := &cacheFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()

	evidence, err := compute(ctx)

	c.mu.Lock()
	flight.evidence = evidence
	flight.err = err
	delete(c.flights, key)
	if err == nil {
		c.evictOneIfFull(now)
		c.entries[key] = cacheEntry{evidence: evidence, expires: now.Add(c.ttl), usedAt: now}
	}
	close(flight.done)
	c.mu.Unlock()
	return evidence, false, err
}

func (c *EvidenceCache) evictOneIfFull(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, key)
		}
	}
	if len(c.entries) < c.maxEntries {
		return
	}
	var oldestKey string
	var oldest time.Time
	for key, entry := range c.entries {
		if oldestKey == "" || entry.usedAt.Before(oldest) {
			oldestKey = key
			oldest = entry.usedAt
		}
	}
	delete(c.entries, oldestKey)
}
