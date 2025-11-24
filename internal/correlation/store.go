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
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
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

// Store is an in-memory key-value store with TTL and LRU eviction.
// It correlates webhook admission events with watch events using sanitized object identity.
// Each key stores a single entry.
type Store struct {
	mu sync.Mutex

	// entries maps correlation keys to entries
	entries map[string]*Entry

	// lruList maintains insertion order for LRU eviction
	lruList *list.List

	// lruMap maps keys to list elements for O(1) removal
	lruMap map[string]*list.Element

	// ttl is the duration after which entries expire
	ttl time.Duration

	// maxEntries is the maximum number of entries before LRU eviction
	maxEntries int

	// logger for warnings
	logger *slog.Logger

	// metrics callbacks (optional)
	onEviction func() // called when an entry is evicted (LRU or TTL)
}

// NewStore creates a new correlation store with the specified TTL and maximum entries.
// ttl: duration after which entries expire (typically 5 minutes).
// maxEntries: maximum number of entries before LRU eviction (typically 10,000).
func NewStore(ttl time.Duration, maxEntries int) *Store {
	return &Store{
		entries:    make(map[string]*Entry),
		lruList:    list.New(),
		lruMap:     make(map[string]*list.Element),
		ttl:        ttl,
		maxEntries: maxEntries,
		logger:     slog.Default(),
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
// If the key already exists and is not expired:
//   - If the username matches, updates the timestamp.
//   - If the username differs and existing is not empty or system:node, logs an info message and ignores the update.
//
// If the key exists but is expired, overwrites the entry.
// If the key does not exist, creates a new entry.
func (s *Store) Put(key string, username string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	entry := &Entry{
		Username:  username,
		Timestamp: now,
	}

	// Check if key already exists
	if existing, exists := s.entries[key]; exists {
		if time.Since(existing.Timestamp) > s.ttl {
			// Entry is expired, overwrite
			s.entries[key] = entry
			s.lruList.MoveToFront(s.lruMap[key])
			return
		}
		// Entry is not expired
		if existing.Username != username && existing.Username != "" &&
			!strings.HasPrefix(existing.Username, "system:node") {
			// Different user and existing is not empty or system:node, ignore and log info
			s.logger.InfoContext(
				context.Background(),
				"ignoring user attribution since it was already claimed",
				"key",
				key,
				"existing_user",
				existing.Username,
				"attempted_user",
				username,
			)
			// Still move to front for LRU
			s.lruList.MoveToFront(s.lruMap[key])
			return
		}
		// Same user or existing is empty or system:node, update/overwrite
		s.entries[key] = entry
		s.lruList.MoveToFront(s.lruMap[key])
		return
	}

	// Check capacity and evict if necessary
	if len(s.entries) >= s.maxEntries {
		s.evictLRU()
	}

	// Create new entry for this key
	s.entries[key] = entry

	// Add to LRU list
	elem := s.lruList.PushFront(key)
	s.lruMap[key] = elem
}

// Get retrieves the correlation entry for the given key.
// Returns the entry and true if found and not expired, or nil and false otherwise.
// Expired entries are automatically removed during lookup.
func (s *Store) Get(key string) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.entries[key]
	if !exists {
		return nil, false
	}

	// Check TTL
	if time.Since(entry.Timestamp) > s.ttl {
		// Entry expired, remove it
		s.removeEntry(key)
		if s.onEviction != nil {
			s.onEviction()
		}
		return nil, false
	}

	// Return entry without removing it
	return entry, true
}

// EvictExpired removes all expired entries from the store.
// This is typically called periodically to prevent unbounded growth.
func (s *Store) EvictExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	evicted := 0

	// Iterate through all entries and remove expired ones
	for key, entry := range s.entries {
		if now.Sub(entry.Timestamp) > s.ttl {
			s.removeEntry(key)
			if s.onEviction != nil {
				s.onEviction()
			}
			evicted++
		}
	}

	return evicted
}

// Size returns the current number of entries in the store.
func (s *Store) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Clear removes all entries from the store.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries = make(map[string]*Entry)
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
