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
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
)

// DefaultAttributionFactTTL is how long an attribution fact is retained in Redis
// waiting for the matching watch event to join it. Facts are never object state, so
// they expire on their own — nothing deletes them. Configurable via --attribution-ttl.
const DefaultAttributionFactTTL = 10 * time.Minute

// watchCursorTTL bounds a stored watch-resume cursor. A live GitTarget refreshes its
// cursor on every watch event and ~minutely bookmark, so the TTL only fires once a
// watch has been gone longer than this — a deleted GitTarget, or a long outage — after
// which the next session safely rebuilds from a fresh replay. The GitTarget UID is part
// of the key, so a recreated target never inherits a stale predecessor's cursor.
const watchCursorTTL = time.Hour

// keyPrefix is the fixed root namespace for every Redis key this index owns.
const keyPrefix = "gitops-reverser"

const (
	attributionKeySuffix     = ":attr:v1:"
	watchCursorKeySuffix     = ":watch-cursor:v1:"
	attributionVariantExact  = "e" // (group, resource, namespace, name, uid, rv)
	attributionVariantUID    = "u" // (group, resource, namespace, name, uid)
	attributionVariantRV     = "r" // (group, resource, namespace, name, rv)
	serviceAccountUserPrefix = "system:serviceaccount:"
)

// commitRequestResource is the plural resource name of the CommitRequest CRD; the
// controller resolves its submitter through this index by (namespace, name, uid).
const commitRequestResource = "commitrequests"

// AuthorFact is the minimal attribution fact stored per accepted, mutating audit
// event and read back by the watch-event resolver. It names an author candidate
// and carries the evidence needed to decide confidence; it is never object state.
type AuthorFact struct {
	Author           string `json:"author"`
	DisplayName      string `json:"displayName,omitempty"`
	Email            string `json:"email,omitempty"`
	Verb             string `json:"verb,omitempty"`
	Subresource      string `json:"subresource,omitempty"`
	AuditID          string `json:"auditID,omitempty"`
	ResourceVersion  string `json:"resourceVersion,omitempty"`
	StageTimestamp   string `json:"stageTimestamp,omitempty"`
	IsServiceAccount bool   `json:"isServiceAccount,omitempty"`
}

// AttributionIndexConfig configures the Redis-backed attribution index.
type AttributionIndexConfig struct {
	Addr       string
	Username   string
	AuthValue  string
	DB         int
	FactTTL    time.Duration
	TLSEnabled bool
}

// AttributionIndex is the Redis-backed lookup table that names a commit author from
// audit facts and persists per-watch resume cursors. It stores only attribution facts
// keyed for a join against watch events and short-lived cursors, never object state.
type AttributionIndex struct {
	client  *redis.Client
	factTTL time.Duration
}

// NewAttributionIndex builds the Redis-backed attribution index.
func NewAttributionIndex(cfg AttributionIndexConfig) (*AttributionIndex, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
	}

	factTTL := cfg.FactTTL
	if factTTL <= 0 {
		factTTL = DefaultAttributionFactTTL
	}

	options := &redis.Options{
		Addr:     cfg.Addr,
		Username: cfg.Username,
		Password: cfg.AuthValue,
		DB:       cfg.DB,
	}
	if cfg.TLSEnabled {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return &AttributionIndex{client: redis.NewClient(options), factTTL: factTTL}, nil
}

// Ping checks liveness of the underlying Redis/Valkey connection. The readiness
// gate uses it so the pod does not join the audit Service before it can store facts.
func (a *AttributionIndex) Ping(ctx context.Context) error {
	return a.client.Ping(ctx).Err()
}

// LookupWatchCursor returns the last resourceVersion durably processed for one
// GitTarget watch shard. A miss means the watch must rebuild from a fresh replay.
func (a *AttributionIndex) LookupWatchCursor(
	ctx context.Context,
	gitTargetNamespace, gitTargetName, gitTargetUID string,
	gvr schema.GroupVersionResource,
	namespace string,
) (string, bool) {
	key := a.watchCursorKey(gitTargetNamespace, gitTargetName, gitTargetUID, gvr, namespace)
	rv, err := a.client.Get(ctx, key).Result()
	if err != nil || rv == "" {
		return "", false
	}
	return rv, true
}

// RecordWatchCursor stores the last resourceVersion durably processed for one
// GitTarget watch shard, refreshing watchCursorTTL on each write. The cursor is keyed
// by GitTarget UID and bounded by the TTL, so it never needs explicit deletion: a live
// watch keeps it fresh, and a dead one's cursor simply expires.
func (a *AttributionIndex) RecordWatchCursor(
	ctx context.Context,
	gitTargetNamespace, gitTargetName, gitTargetUID string,
	gvr schema.GroupVersionResource,
	namespace, rv string,
) error {
	if rv == "" {
		return nil
	}
	key := a.watchCursorKey(gitTargetNamespace, gitTargetName, gitTargetUID, gvr, namespace)
	if err := a.client.Set(ctx, key, rv, watchCursorTTL).Err(); err != nil {
		return fmt.Errorf("store watch cursor: %w", err)
	}
	return nil
}

// RecordFact stores the attribution fact for one accepted, mutating audit event
// under every join key it can compute (exact, uid, rv), each with a bounded TTL.
// It is a no-op for events without an objectRef, a resolvable name, or a user —
// those can never name an author. The caller (the audit handler) has already
// rejected reads, failures, dry-runs, and non-ResponseComplete stages.
func (a *AttributionIndex) RecordFact(ctx context.Context, event auditv1.Event) error {
	if event.ObjectRef == nil {
		return nil
	}
	group := event.ObjectRef.APIGroup
	resource := event.ObjectRef.Resource
	if resource == "" {
		return nil
	}

	op, _ := auditutil.VerbToOperation(event.Verb)
	identity := auditutil.IdentityFromAuditEvent(event, op)
	if identity.Name == "" {
		return nil
	}

	user := resolveUserInfo(event)
	if user.Username == "" {
		return nil
	}

	fact := AuthorFact{
		Author:           user.Username,
		DisplayName:      user.DisplayName,
		Email:            user.Email,
		Verb:             event.Verb,
		Subresource:      event.ObjectRef.Subresource,
		AuditID:          string(event.AuditID),
		ResourceVersion:  resourceVersionFromEvent(event),
		IsServiceAccount: strings.HasPrefix(user.Username, serviceAccountUserPrefix),
	}
	if !event.StageTimestamp.IsZero() {
		fact.StageTimestamp = event.StageTimestamp.UTC().Format(time.RFC3339Nano)
	}

	raw, err := json.Marshal(fact)
	if err != nil {
		return fmt.Errorf("marshal attribution fact: %w", err)
	}

	for _, key := range a.factKeyVariants(group, resource, identity.Namespace, identity.Name, identity.UID, fact.ResourceVersion) {
		if err := a.client.Set(ctx, key, raw, a.factTTL).Err(); err != nil {
			return fmt.Errorf("store attribution fact %q: %w", key, err)
		}
	}
	return nil
}

// LookupAuthor finds the strongest attribution fact for a watch event, trying the
// exact (uid+rv), uid-only, then rv-only join keys in that order. ok=false means no
// fact matched (yet) — the caller ships as committer.
func (a *AttributionIndex) LookupAuthor(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace, name string,
	uid types.UID,
	rv string,
) (AuthorFact, bool) {
	for _, key := range a.factKeyVariants(gvr.Group, gvr.Resource, namespace, name, uid, rv) {
		raw, err := a.client.Get(ctx, key).Bytes()
		if err != nil {
			continue
		}
		var fact AuthorFact
		if err := json.Unmarshal(raw, &fact); err != nil {
			continue
		}
		if fact.Author == "" {
			continue
		}
		return fact, true
	}
	return AuthorFact{}, false
}

// LookupCommitRequestAuthor reads the CommitRequest create-author captured at audit
// ingestion, keyed by (namespace, name, uid). ok=false means the create event has
// not been observed yet (the webhook may still be ingesting it) or a transient miss;
// the controller retries on its own cadence and finalizes as committer past its bound.
func (a *AttributionIndex) LookupCommitRequestAuthor(
	ctx context.Context, namespace, name string, uid types.UID,
) (string, bool) {
	gvr := schema.GroupVersionResource{Group: configv1alpha2.GroupVersion.Group, Resource: commitRequestResource}
	fact, ok := a.LookupAuthor(ctx, gvr, namespace, name, uid, "")
	if !ok {
		return "", false
	}
	return fact.Author, fact.Author != ""
}

// factKeyVariants returns the join keys for an event/lookup, strongest first. A
// variant is only emitted when its inputs are present, so a record writes (and a
// lookup reads) exactly the keys that are computable. Writes and reads share this
// function so they can never drift.
func (a *AttributionIndex) factKeyVariants(group, resource, namespace, name string, uid types.UID, rv string) []string {
	// At most three variants: exact (uid+rv), uid-only, rv-only.
	const maxFactKeyVariants = 3
	keys := make([]string, 0, maxFactKeyVariants)
	if uid != "" && rv != "" {
		keys = append(keys, a.factKey(attributionVariantExact,
			group, resource, namespace, name, string(uid), rv))
	}
	if uid != "" {
		keys = append(keys, a.factKey(attributionVariantUID,
			group, resource, namespace, name, string(uid)))
	}
	if rv != "" {
		keys = append(keys, a.factKey(attributionVariantRV,
			group, resource, namespace, name, rv))
	}
	return keys
}

func (a *AttributionIndex) factKey(variant string, parts ...string) string {
	id := hex.EncodeToString([]byte(strings.Join(parts, "\x00")))
	return keyPrefix + attributionKeySuffix + variant + ":" + id
}

func (a *AttributionIndex) watchCursorKey(
	gitTargetNamespace, gitTargetName, gitTargetUID string,
	gvr schema.GroupVersionResource,
	namespace string,
) string {
	id := hex.EncodeToString([]byte(strings.Join([]string{
		gitTargetNamespace,
		gitTargetName,
		gitTargetUID,
		gvr.Group,
		gvr.Version,
		gvr.Resource,
		namespace,
	}, "\x00")))
	return keyPrefix + watchCursorKeySuffix + id
}

// resourceVersionFromEvent returns the event's ResourceVersion when one is available,
// or "" when it is not (deletes, collection verbs, shallow bodies). The post-write RV
// lives in the response object's metadata.resourceVersion; requestObject.resourceVersion
// is the pre-write RV on update-style requests, so it is intentionally ignored.
// objectRef.resourceVersion is usually the empty precondition RV on writes, so it is only
// the last resort.
func resourceVersionFromEvent(event auditv1.Event) string {
	if rv := rvFromRawObject(event.ResponseObject); rv != "" {
		return rv
	}
	if event.ObjectRef != nil {
		return event.ObjectRef.ResourceVersion
	}
	return ""
}

func rvFromRawObject(obj *runtime.Unknown) string {
	if obj == nil || len(obj.Raw) == 0 {
		return ""
	}
	var probe struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(obj.Raw, &probe); err != nil {
		return ""
	}
	return probe.Metadata.ResourceVersion
}
