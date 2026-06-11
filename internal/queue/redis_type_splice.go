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
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
)

// ErrSpliceNoCheckpoint is returned when a type has no pinned checkpoint revision yet
// (:objects:rv absent or blank). The splice is fail-closed on it: an absent checkpoint is NEVER
// an empty desired set, so the caller must hold rather than sweep (R11). In practice the watch
// layer gates on the Materializer's Synced phase before splicing, so this is a defensive backstop.
var ErrSpliceNoCheckpoint = errors.New("type-splice: no checkpoint revision pinned")

// RedisTypeSplicer folds a type's checkpoint (:objects:items @ :objects:rv) with the audit-log
// entries strictly after that revision into the current desired object set — the read side of the
// api-source-of-truth splice (docs/design/stream/api-source-of-truth-reconcile.md §6). It shares
// the per-type base key "<prefix>:<group-or-core>:<resource>" with the objects snapshot
// (RedisObjectsSnapshot) and the audit stream (RedisByTypeStreamQueue), reusing the audit
// consumer's extractObject/sanitize path verbatim for the audit-body→object conversion.
type RedisTypeSplicer struct {
	client *redis.Client
	prefix string
}

// NewRedisTypeSplicer creates a splice reader bound to the same per-type keyspace the objects
// snapshot and audit mirror write. It reuses RedisObjectsSnapshotConfig because it reads the same
// keys under the same prefix and connection settings.
func NewRedisTypeSplicer(cfg RedisObjectsSnapshotConfig) (*RedisTypeSplicer, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
	}

	prefix := strings.TrimSpace(cfg.Prefix)
	if prefix == "" {
		prefix = DefaultRedisByTypeStreamPrefix
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

	return &RedisTypeSplicer{client: redis.NewClient(options), prefix: prefix}, nil
}

// SpliceType returns the current desired object set for one resource type and the checkpoint
// revision it was anchored at. It reads the checkpoint @R, then folds every audit-log entry with
// stream position strictly after R (an exclusive "(R +" XRANGE — exact under async delivery because
// position is the resourceVersion, not arrival time, DEC-3): a delete drops the identity, any other
// mutating verb upserts the extracted object, last-writer-wins by stream order (§6). The returned
// objects are sanitized and sorted by identity for a deterministic plan; they are NOT scope-filtered
// (the caller restricts them to the GitTarget's namespaces). A missing checkpoint returns
// ErrSpliceNoCheckpoint so the caller holds (R11); a malformed checkpoint entry or an unconvertible
// audit body is skipped best-effort, since the next checkpoint backstops it (DEC-5).
func (s *RedisTypeSplicer) SpliceType(
	ctx context.Context,
	group, resource string,
) ([]*unstructured.Unstructured, string, error) {
	base := typeBaseKey(s.prefix, group, resource, "")

	rv, err := s.client.Get(ctx, base+objectsRVSuffix).Result()
	if errors.Is(err, redis.Nil) || strings.TrimSpace(rv) == "" {
		return nil, "", ErrSpliceNoCheckpoint
	}
	if err != nil {
		return nil, "", fmt.Errorf("type-splice: read checkpoint rv for %q: %w", base, err)
	}

	items, err := s.client.HGetAll(ctx, base+objectsItemsSuffix).Result()
	if err != nil {
		return nil, "", fmt.Errorf("type-splice: read checkpoint items for %q: %w", base, err)
	}
	desired := make(map[string]*unstructured.Unstructured, len(items))
	for identity, envJSON := range items {
		obj, decErr := decodeCheckpointObject(envJSON)
		if decErr != nil {
			// A malformed checkpoint entry is skipped; the next checkpoint re-fill heals it.
			continue
		}
		desired[identity] = obj
	}

	// Fold the log strictly after the checkpoint revision. "(R" is an exclusive start, so the
	// rv==R entries the checkpoint already contains are skipped; "+" reads to the stream head.
	msgs, err := s.client.XRange(ctx, base+byTypeAuditStreamSuffix, "("+rv, "+").Result()
	if err != nil {
		return nil, "", fmt.Errorf("type-splice: read audit log for %q: %w", base, err)
	}
	for i := range msgs {
		foldAuditEntry(desired, msgs[i].Values)
	}

	return sortedDesiredObjects(desired), rv, nil
}

// foldAuditEntry applies one audit-log entry to the desired set: a delete drops the identity, any
// other mutating verb upserts the extracted (sanitized) object. It reuses the consumer's
// parseAuditEvent / VerbToOperation / effectiveAuditOperation / extractObject path verbatim, so the
// spliced object is byte-identical to what the live pipeline would have written. A non-mutating
// verb, a missing objectRef, a subresource body, or an unconvertible body (status/partial/missing)
// is skipped — the checkpoint backstops correctness (DEC-5).
func foldAuditEntry(desired map[string]*unstructured.Unstructured, values map[string]interface{}) {
	event, err := parseAuditEvent(values)
	if err != nil {
		return
	}
	if event.Stage != auditv1.StageResponseComplete || event.ObjectRef == nil {
		return
	}
	op, ok := auditutil.VerbToOperation(event.Verb)
	if !ok {
		return
	}
	op = effectiveAuditOperation(event, op)

	id := auditutil.IdentityFromAuditEvent(event, op)
	identity := spliceIdentity(id.Namespace, id.Name)
	if identity == "" {
		return
	}
	if op == configv1alpha1.OperationDelete {
		delete(desired, identity)
		return
	}

	ref := event.ObjectRef
	if ref.Subresource != "" {
		// A parent-stream scale entry (DEC-A, canonical-stream-retirement.md) folds into the
		// parent's desired object. The splice MUST fold scale: otherwise a correctness
		// reconcile would rebuild desired from checkpoint@R, see replicas@R, and REVERT the
		// live scale the freshness tail already wrote — flip-flopping against it (§5 of the
		// retirement doc). A parent not in desired yet, an unknown replica path, or any
		// other subresource is skipped; the next checkpoint backstops (DEC-5).
		foldScaleEntry(desired, event, identity)
		return
	}
	apiGroup, apiVersion := auditutil.ObjectRefGroupVersion(ref)
	fullAPIVersion := apiVersion
	if apiGroup != "" {
		fullAPIVersion = apiGroup + "/" + apiVersion
	}
	obj, err := extractObject(event, op, fullAPIVersion, ref.Resource, id.Namespace, id.Name)
	if err != nil {
		// Best-effort: a metav1.Status body, a partial patch fragment, or a missing body is
		// dropped here; the next checkpoint LIST reflects the object's true state (DEC-5).
		return
	}
	desired[identity] = obj
}

// foldScaleEntry applies a parent-stream scale entry to the desired set: it mutates the
// already-present parent object's replicas at the parent's known replica path, using the
// same translation the freshness tail uses (translateScaleToAssignments), so the splice's
// desired set agrees with what the tail already wrote. Best-effort by design: a non-scale
// subresource, an unknown replica path (CRD/aggregated parent), a missing Scale body, or
// a parent absent from desired all skip — the next checkpoint LIST reflects the parent's
// true state (DEC-5).
func foldScaleEntry(desired map[string]*unstructured.Unstructured, event auditv1.Event, identity string) {
	obj, present := desired[identity]
	if !present {
		return
	}
	ref := event.ObjectRef
	apiGroup, _ := auditutil.ObjectRefGroupVersion(ref)
	assignments, _, ok := translateScaleToAssignments(event, apiGroup, ref.Resource)
	if !ok {
		return
	}
	for _, a := range assignments {
		if err := unstructured.SetNestedField(obj.Object, a.Value, a.Path...); err != nil {
			return
		}
	}
}

// decodeCheckpointObject extracts the stored object body from one :objects:items envelope. The
// envelope schema is owned by the writer (internal/watch objectEnvelope); only its "object" field
// — the sanitized body — is needed here.
func decodeCheckpointObject(envJSON string) (*unstructured.Unstructured, error) {
	var env struct {
		Object json.RawMessage `json:"object"`
	}
	if err := json.Unmarshal([]byte(envJSON), &env); err != nil {
		return nil, fmt.Errorf("decode checkpoint envelope: %w", err)
	}
	if len(env.Object) == 0 {
		return nil, errors.New("checkpoint envelope has no object body")
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(env.Object); err != nil {
		return nil, fmt.Errorf("decode checkpoint object body: %w", err)
	}
	return obj, nil
}

// spliceIdentity builds the fold key for one object, matching the checkpoint HASH field written by
// objectIdentity: "<namespace>/<name>" for namespaced objects, "<name>" for cluster-scoped ones.
// A blank name (an unresolved identity) yields "" so the caller drops the entry.
func spliceIdentity(namespace, name string) string {
	if name == "" {
		return ""
	}
	if namespace != "" {
		return namespace + "/" + name
	}
	return name
}

// sortedDesiredObjects flattens the folded desired map into a slice ordered by identity, so a
// reconcile builds a deterministic plan regardless of Redis hash iteration order.
func sortedDesiredObjects(desired map[string]*unstructured.Unstructured) []*unstructured.Unstructured {
	ids := make([]string, 0, len(desired))
	for id := range desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*unstructured.Unstructured, 0, len(ids))
	for _, id := range ids {
		out = append(out, desired[id])
	}
	return out
}
