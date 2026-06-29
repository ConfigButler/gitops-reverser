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

package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/types"
)

// commandAuthorKeySuffix namespaces the keys this store owns. It shares the top-level
// author domain with audit-sourced resource facts, but the command subfamily has a
// different provenance and no grace-window join.
const commandAuthorKeySuffix = ":author:v1:command:"

// commandAuthorRecordTTL bounds a captured authorship record. It is NOT tuned to cover
// any wait: by the authorship invariant the record is written before the object exists,
// and the controller reads it on the first reconcile after persist — typically
// sub-second. The TTL exists ONLY so an orphan record (a command object deleted before
// its reconcile) self-cleans. It is a fixed internal constant, deliberately not a flag:
// there is nothing to tune. An hour is generous headroom for a slow reconcile backlog or
// a leader failover.
const commandAuthorRecordTTL = time.Hour

// CommandAuthor is the minimal authorship captured at admission for one command object.
// It carries only what a git commit author needs — no RV, no auditID, no conflict bit:
// this is a 1:1 command capture, not a post-persist join.
type CommandAuthor struct {
	Author      string `json:"author"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	RequestedAt string `json:"requestedAt,omitempty"` // RFC3339Nano, for lag metrics/debug
}

// CommandAuthorStore records and reads command authorship. It shares RedisStore's
// connection but is wired whenever the internal-commands webhook is enabled —
// independent of --author-attribution, which only governs mirrored-resource attribution.
type CommandAuthorStore struct {
	client *redis.Client
	ttl    time.Duration
}

// RecordCommandAuthor is the admission-side write: capture the authenticated submitter
// the instant a command CREATE is admitted, before it persists. Last-write-wins (a
// CREATE fires admission once; a retried admission re-asserts the same user).
func (s *CommandAuthorStore) RecordCommandAuthor(
	ctx context.Context, uid types.UID, author CommandAuthor,
) error {
	raw, err := json.Marshal(author)
	if err != nil {
		return fmt.Errorf("marshal command author: %w", err)
	}
	return s.client.Set(ctx, s.key(uid), raw, s.ttl).Err()
}

// LookupCommandAuthor is the controller-side read, keyed by the persisted object's UID.
// ok=false means no record was captured — the internal-commands webhook is not
// configured (or a best-effort write missed) — and the controller finalizes as the
// committer, immediately.
func (s *CommandAuthorStore) LookupCommandAuthor(
	ctx context.Context, uid types.UID,
) (CommandAuthor, bool) {
	raw, err := s.client.Get(ctx, s.key(uid)).Bytes()
	if err != nil {
		return CommandAuthor{}, false
	}
	var a CommandAuthor
	if json.Unmarshal(raw, &a) != nil || a.Author == "" {
		return CommandAuthor{}, false
	}
	return a, true
}

// key identifies the command by UID alone — globally unique, like the watch cursor key,
// so namespace/name/kind would be redundant (kept out for a tight key).
func (s *CommandAuthorStore) key(uid types.UID) string {
	return keyPrefix + commandAuthorKeySuffix + escapeKeyField(string(uid))
}
