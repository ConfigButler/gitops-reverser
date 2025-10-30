/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package correlation provides an in-memory key-value store for correlating
// webhook admission events with watch events via sanitized object identity.
package correlation

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// Entry represents a stored correlation entry with username and timestamp.
type Entry struct {
	Username  string
	Timestamp time.Time
}

// entryQueue holds multiple entries for the same key in FIFO order.
// This handles rapid changes to the same content by different users.
type entryQueue struct {
	entries []*Entry
}

// Store is an in-memory key-value store with TTL and LRU eviction.
// It correlates webhook admission events with watch events using sanitized object identity.
// Each key maintains a FIFO queue of entries to handle rapid changes to same content.
type Store struct {
	mu sync.Mutex

	// entries maps correlation keys to their entry queues
	entries map[string]*entryQueue

	// lruList maintains insertion order for LRU eviction
	lruList *list.List

	// lruMap maps keys to list elements for O(1) removal
	lruMap map[string]*list.Element

	// ttl is the duration after which entries expire
	ttl time.Duration

	// maxEntries is the maximum number of entries before LRU eviction
	maxEntries int

	// maxQueueDepth is the maximum number of entries per key
	maxQueueDepth int

	// metrics callbacks (optional)
	onEviction func() // called when an entry is evicted (LRU or TTL)
}

// NewStore creates a new correlation store with the specified TTL and maximum entries.
// ttl: duration after which entries expire (typically 60 seconds).
// maxEntries: maximum number of entries before LRU eviction (typically 10,000).
func NewStore(ttl time.Duration, maxEntries int) *Store {
	const defaultMaxQueueDepth = 10 // Max webhook events per key before dropping oldest
	return &Store{
		entries:       make(map[string]*entryQueue),
		lruList:       list.New(),
		lruMap:        make(map[string]*list.Element),
		ttl:           ttl,
		maxEntries:    maxEntries,
		maxQueueDepth: defaultMaxQueueDepth,
	}
}

// SetEvictionCallback sets a callback function that is invoked when an entry is evicted.
// This is typically used to increment eviction metrics.
func (s *Store) SetEvictionCallback(callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onEviction = callback
}

// GenerateKey creates a correlation key from a resource identifier, operation, and sanitized content hash.
// Key format: {group}/{version}/{resource}/{namespace}/{name}:{operation}:{specHash}
// where specHash is the 16-character hex representation of xxhash64(sanitizedYAML).
func GenerateKey(id types.ResourceIdentifier, operation string, sanitizedYAML []byte) string {
	const (
		hashByteSize = 8  // xxhash64 produces 8 bytes
		hexCharSize  = 16 // 8 bytes = 16 hex characters
	)

	// Compute xxhash64 of sanitized YAML (much faster than SHA256)
	h := xxhash.Sum64(sanitizedYAML)
	// Convert to 16-char hex string
	hashBytes := make([]byte, hashByteSize)
	binary.BigEndian.PutUint64(hashBytes, h)
	hashStr := make([]byte, hexCharSize)
	const hexTable = "0123456789abcdef"
	for i := range hashByteSize {
		hashStr[i*2] = hexTable[hashBytes[i]>>4]
		hashStr[i*2+1] = hexTable[hashBytes[i]&0x0f]
	}

	// Build key based on resource structure
	hashString := string(hashStr)
	if id.Namespace == "" {
		// Cluster-scoped resource
		if id.Group == "" {
			// Core resource without group
			return fmt.Sprintf("%s/%s/%s:%s:%s",
				id.Version, id.Resource, id.Name, operation, hashString)
		}
		return fmt.Sprintf("%s/%s/%s/%s:%s:%s",
			id.Group, id.Version, id.Resource, id.Name, operation, hashString)
	}
	// Namespaced resource
	if id.Group == "" {
		// Core resource without group
		return fmt.Sprintf("%s/%s/%s/%s:%s:%s",
			id.Version, id.Resource, id.Namespace, id.Name, operation, hashString)
	}
	return fmt.Sprintf("%s/%s/%s/%s/%s:%s:%s",
		id.Group, id.Version, id.Resource, id.Namespace, id.Name, operation, hashString)
}

// Put stores a correlation entry for the given key.
// Entries are queued in FIFO order to handle rapid changes to the same content.
// If the queue for this key is at max depth, the oldest entry is dropped.
// If the store is at capacity, it evicts the least recently used key.
func (s *Store) Put(key string, username string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	entry := &Entry{
		Username:  username,
		Timestamp: now,
	}

	// Check if key already has a queue
	if queue, exists := s.entries[key]; exists {
		// Append to existing queue (FIFO)
		queue.entries = append(queue.entries, entry)

		// Enforce max queue depth per key
		if len(queue.entries) > s.maxQueueDepth {
			// Drop oldest entry from queue
			queue.entries = queue.entries[1:]
			if s.onEviction != nil {
				s.onEviction()
			}
		}

		// Move to front of LRU list (this key was accessed)
		s.lruList.MoveToFront(s.lruMap[key])
		return
	}

	// Check capacity and evict if necessary
	if len(s.entries) >= s.maxEntries {
		s.evictLRU()
	}

	// Create new queue for this key
	s.entries[key] = &entryQueue{
		entries: []*Entry{entry},
	}

	// Add to LRU list
	elem := s.lruList.PushFront(key)
	s.lruMap[key] = elem
}

// GetAndDelete retrieves and removes the oldest correlation entry for the given key.
// Uses FIFO ordering: the first entry enqueued is the first retrieved.
// Returns the entry and true if found and not expired, or nil and false otherwise.
// Expired entries are automatically removed during lookup.
func (s *Store) GetAndDelete(key string) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue, exists := s.entries[key]
	if !exists || len(queue.entries) == 0 {
		return nil, false
	}

	// Get oldest entry from queue (FIFO)
	entry := queue.entries[0]

	// Check TTL
	if time.Since(entry.Timestamp) > s.ttl {
		// Entry expired, remove entire queue for this key
		s.removeEntry(key)
		if s.onEviction != nil {
			s.onEviction()
		}
		return nil, false
	}

	// Remove entry from queue
	queue.entries = queue.entries[1:]

	// If queue is now empty, remove the key entirely
	if len(queue.entries) == 0 {
		s.removeEntry(key)
	}

	return entry, true
}

// EvictExpired removes all expired entries from the store.
// This is typically called periodically to prevent unbounded growth.
func (s *Store) EvictExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	evicted := 0

	// Iterate through all queues and remove expired ones
	for key, queue := range s.entries {
		if len(queue.entries) == 0 {
			continue
		}
		// Check oldest entry in queue
		if now.Sub(queue.entries[0].Timestamp) > s.ttl {
			s.removeEntry(key)
			if s.onEviction != nil {
				// Count all entries in the queue as evicted
				evicted += len(queue.entries)
				for range queue.entries {
					s.onEviction()
				}
			} else {
				evicted += len(queue.entries)
			}
		}
	}

	return evicted
}

// Size returns the current number of entries in the store (across all queues).
func (s *Store) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, queue := range s.entries {
		total += len(queue.entries)
	}
	return total
}

// Clear removes all entries from the store.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries = make(map[string]*entryQueue)
	s.lruList = list.New()
	s.lruMap = make(map[string]*list.Element)
}

// evictLRU removes the least recently used entry.
// Must be called with lock held.
func (s *Store) evictLRU() {
	if s.lruList.Len() == 0 {
		return
	}

	// Get the oldest element (back of list)
	oldest := s.lruList.Back()
	if oldest == nil {
		return
	}

	key, ok := oldest.Value.(string)
	if !ok {
		return
	}
	s.removeEntry(key)

	if s.onEviction != nil {
		s.onEviction()
	}
}

// removeEntry removes an entry from the store and LRU tracking.
// Must be called with lock held.
func (s *Store) removeEntry(key string) {
	delete(s.entries, key)

	if elem, exists := s.lruMap[key]; exists {
		s.lruList.Remove(elem)
		delete(s.lruMap, key)
	}
}
