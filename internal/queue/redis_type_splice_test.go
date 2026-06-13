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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

// spliceFixture wires a splicer, an objects-snapshot writer, and an audit-stream writer onto the
// shared Valkey container under one prefix, so a test can build a checkpoint + log and read the
// fold back. It skips when no real Valkey is available (the exclusive "(R +" XRANGE and RV-ordered
// IDs are real-Valkey semantics, §9).
func spliceFixture(t *testing.T) (*RedisTypeSplicer, *RedisObjectsSnapshot, *RedisByTypeStreamQueue) {
	t.Helper()
	if sharedValkeyAddr == "" {
		t.Skip("real-Valkey container unavailable (Docker required); skipping splice test")
	}
	prefix := fmt.Sprintf("test.splice.%d", valkeyPrefixSeq.Add(1))
	cfg := RedisObjectsSnapshotConfig{Addr: sharedValkeyAddr, Prefix: prefix}
	splicer, err := NewRedisTypeSplicer(cfg)
	require.NoError(t, err)
	snap, err := NewRedisObjectsSnapshot(cfg)
	require.NoError(t, err)
	q, err := NewRedisByTypeStreamQueue(RedisByTypeStreamConfig{Addr: sharedValkeyAddr, Prefix: prefix})
	require.NoError(t, err)
	return splicer, snap, q
}

// checkpointEnvelope builds one :objects:items value with data k=v1 (the pre-update checkpoint
// state): only the "object" field (the sanitized body) is read by the splice, matching the writer's
// objectEnvelope schema.
func checkpointEnvelope(name string) string {
	body := fmt.Sprintf(
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":%q,"namespace":"default"},"data":{"k":"v1"}}`,
		name)
	env, _ := json.Marshal(map[string]any{"object": json.RawMessage(body)})
	return string(env)
}

// configMapAuditEvent builds a mutating ConfigMap audit event carrying the post-write body, the
// shape the per-type stream stores. A delete carries no body — identity comes from the objectRef.
func configMapAuditEvent(verb, name, rv, data string) auditv1.Event {
	e := auditv1.Event{
		Verb:           verb,
		Stage:          auditv1.StageResponseComplete,
		StageTimestamp: metav1.MicroTime{Time: time.Now()},
		ObjectRef: &auditv1.ObjectReference{
			APIVersion: "v1",
			Resource:   "configmaps",
			Namespace:  "default",
			Name:       name,
		},
	}
	if verb != "delete" {
		body := fmt.Sprintf(
			`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":%q,"namespace":"default",`+
				`"resourceVersion":%q},"data":{"k":%q}}`,
			name, rv, data)
		e.ResponseObject = &runtime.Unknown{Raw: []byte(body)}
	}
	return e
}

func configMapData(t *testing.T, obj map[string]interface{}) string {
	t.Helper()
	data, ok := obj["data"].(map[string]interface{})
	require.True(t, ok, "object has a data block")
	return fmt.Sprintf("%v", data["k"])
}

// TestRedisTypeSplicer_FoldsCheckpointAndLog is the core R2 read: a checkpoint @100 folded with
// log entries after it — an update, a create, and a delete — yields the current desired set,
// last-writer-wins by RV order, pinned at the checkpoint revision.
func TestRedisTypeSplicer_FoldsCheckpointAndLog(t *testing.T) {
	splicer, snap, q := spliceFixture(t)
	ctx := context.Background()

	require.NoError(t, snap.ReplaceTypeObjects(ctx, "", "v1", "configmaps", map[string]string{
		"default/a": checkpointEnvelope("a"),
		"default/b": checkpointEnvelope("b"),
	}, "100"))

	// Log strictly after the checkpoint: update a (v1->v2), create c, delete b.
	require.NoError(t, q.Enqueue(ctx, configMapAuditEvent("update", "a", "150", "v2")))
	require.NoError(t, q.Enqueue(ctx, configMapAuditEvent("create", "c", "160", "vc")))
	require.NoError(t, q.Enqueue(ctx, configMapAuditEvent("delete", "b", "170", "")))

	objs, rv, hc, err := splicer.SpliceType(ctx, "", "configmaps")
	require.NoError(t, err)
	assert.Equal(t, "100", rv, "the splice is anchored at the checkpoint revision")
	// The delete carries no body and so no rv: it ingests rv-less, attached to the stream high-water
	// (the create @160), as "160-1" — not at 170. The coverage head is the full stream position of
	// that last entry, exactly where the tail reads it, so the gate stays consistent down to the seq.
	assert.Equal(t, "160-1", hc, "coverage head is the last main-stream position (the rv-less delete rides 160-1)")

	require.Len(t, objs, 2, "b was deleted; a and c remain")
	assert.Equal(t, "default/a", objs[0].GetNamespace()+"/"+objs[0].GetName(), "sorted by identity")
	assert.Equal(t, "default/c", objs[1].GetNamespace()+"/"+objs[1].GetName())
	assert.Equal(t, "v2", configMapData(t, objs[0].Object), "a reflects the post-checkpoint update")
	assert.Equal(t, "vc", configMapData(t, objs[1].Object), "c was created from the log")
}

// TestRedisTypeSplicer_CoverageHeadGatesTailReplay is the queue half of the per-target watermark
// red-first proof (signing-snapshot-tail-replay-failure-investigation.md §8): a checkpoint @R0 with
// one post-checkpoint create @R1 must expose a coverage head Hc == R1, NOT the checkpoint R0. The
// watch-layer fan-out gates the audit tail on this Hc, so returning R0 here would re-route the very
// log entry the splice just folded as a stray live per-event commit. The checkpoint rv stays R0 for
// commit-message continuity.
func TestRedisTypeSplicer_CoverageHeadGatesTailReplay(t *testing.T) {
	splicer, snap, q := spliceFixture(t)
	ctx := context.Background()

	require.NoError(t, snap.ReplaceTypeObjects(ctx, "", "v1", "configmaps", map[string]string{
		"default/seed": checkpointEnvelope("seed"),
	}, "100"))

	// One create strictly after the checkpoint — exactly the batch-cm-2 case from §4.
	require.NoError(t, q.Enqueue(ctx, configMapAuditEvent("create", "batch-cm-2", "117", "vc")))

	objs, rv, hc, err := splicer.SpliceType(ctx, "", "configmaps")
	require.NoError(t, err)
	assert.Equal(t, "100", rv, "checkpoint rv is unchanged — it still serves commit-message continuity")
	assert.Equal(t, "117-0", hc, "coverage head is the post-checkpoint create's position; rv 100 would re-route it")
	require.Len(t, objs, 2, "seed + the folded create are both in the desired set")
}

// TestRedisTypeSplicer_DuplicateDeliveryFoldsOnce is the C-C duplicate-absorption proof
// (canonical-stream-retirement.md §4.1/§9): with the webhook's auditID decision key retired, a
// doubly-delivered audit event re-mirrors at the SAME resourceVersion (a fresh sub-sequence of
// the same stream ID — never the late lane). The fold is last-writer-wins by position and both
// entries decode to the same object, so the spliced desired set is identical to single delivery:
// one object, one Git effect. This is the same argument that retired content hashing (R7).
func TestRedisTypeSplicer_DuplicateDeliveryFoldsOnce(t *testing.T) {
	splicer, snap, q := spliceFixture(t)
	ctx := context.Background()

	require.NoError(t, snap.ReplaceTypeObjects(ctx, "", "v1", "configmaps", map[string]string{
		"default/a": checkpointEnvelope("a"),
	}, "100"))

	// The same post-checkpoint update delivered twice (a webhook retry).
	require.NoError(t, q.Enqueue(ctx, configMapAuditEvent("update", "a", "150", "v2")))
	require.NoError(t, q.Enqueue(ctx, configMapAuditEvent("update", "a", "150", "v2")))

	objs, rv, hc, err := splicer.SpliceType(ctx, "", "configmaps")
	require.NoError(t, err)
	assert.Equal(t, "100", rv)
	assert.Equal(t, "150-1", hc, "both deliveries land at rv 150 with fresh seqs; the coverage head is the last, 150-1")

	require.Len(t, objs, 1, "a duplicate delivery must not produce a second desired object")
	assert.Equal(t, "default/a", objs[0].GetNamespace()+"/"+objs[0].GetName())
	assert.Equal(t, "v2", configMapData(t, objs[0].Object),
		"both entries fold to the same content — zero extra Git effect")
}

// deploymentCheckpointEnvelope builds one :objects:items value for an apps/v1 Deployment at the
// given replica count — the pre-scale checkpoint state the scale fold must override.
func deploymentCheckpointEnvelope(name string, replicas int) string {
	body := fmt.Sprintf(
		`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":%q,"namespace":"default"},`+
			`"spec":{"replicas":%d}}`, name, replicas)
	env, _ := json.Marshal(map[string]any{"object": json.RawMessage(body)})
	return string(env)
}

// deploymentScaleAuditEvent builds a deployments/scale audit event whose Scale body carries the
// parent's post-scale RV and the accepted replica count — the parent-stream entry DEC-A produces.
func deploymentScaleAuditEvent(name, rv string, replicas int) auditv1.Event {
	return auditv1.Event{
		Verb:           "patch",
		Stage:          auditv1.StageResponseComplete,
		StageTimestamp: metav1.MicroTime{Time: time.Now()},
		ObjectRef: &auditv1.ObjectReference{
			APIGroup:    "apps",
			APIVersion:  "v1",
			Resource:    "deployments",
			Subresource: "scale",
			Namespace:   "default",
			Name:        name,
		},
		ResponseObject: &runtime.Unknown{Raw: []byte(fmt.Sprintf(
			`{"kind":"Scale","apiVersion":"autoscaling/v1",`+
				`"metadata":{"name":%q,"namespace":"default","resourceVersion":%q},`+
				`"spec":{"replicas":%d},"status":{"replicas":0}}`, name, rv, replicas))},
	}
}

// TestRedisTypeSplicer_FoldsScaleIntoParent is the splice half of DEC-A
// (canonical-stream-retirement.md §5): a parent-stream scale entry after the checkpoint mutates
// the parent's desired spec.replicas — without the fold, a correctness reconcile would revert the
// live scale to the checkpoint value and flip-flop against the freshness tail. A scale for a
// parent absent from desired is skipped (next checkpoint backstops it, DEC-5).
func TestRedisTypeSplicer_FoldsScaleIntoParent(t *testing.T) {
	splicer, snap, q := spliceFixture(t)
	ctx := context.Background()

	require.NoError(t, snap.ReplaceTypeObjects(ctx, "apps", "v1", "deployments", map[string]string{
		"default/web": deploymentCheckpointEnvelope("web", 1),
	}, "100"))

	require.NoError(t, q.Enqueue(ctx, deploymentScaleAuditEvent("web", "150", 5)))
	require.NoError(t, q.Enqueue(ctx, deploymentScaleAuditEvent("ghost", "160", 7))) // parent not in desired

	objs, rv, hc, err := splicer.SpliceType(ctx, "apps", "deployments")
	require.NoError(t, err)
	assert.Equal(t, "100", rv)
	assert.Equal(t, "160-0", hc, "coverage head reaches the last log position read (the skipped ghost scale at 160-0)")

	require.Len(t, objs, 1, "the absent-parent scale folds nothing in")
	replicas, found, err := unstructured.NestedInt64(objs[0].Object, "spec", "replicas")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(5), replicas, "the scale entry overrides the checkpoint's replicas")
}

// TestRedisTypeSplicer_NoCheckpointHolds proves the fail-closed contract: with no pinned
// checkpoint, the splice returns ErrSpliceNoCheckpoint (never an empty desired set), so a reconcile
// holds rather than sweeping.
func TestRedisTypeSplicer_NoCheckpointHolds(t *testing.T) {
	splicer, _, q := spliceFixture(t)
	ctx := context.Background()

	// Audit entries exist, but no checkpoint has been pinned.
	require.NoError(t, q.Enqueue(ctx, configMapAuditEvent("create", "a", "10", "v1")))

	_, _, _, err := splicer.SpliceType(ctx, "", "configmaps")
	require.ErrorIs(t, err, ErrSpliceNoCheckpoint)
}

// TestRedisTypeSplicer_EmptyLogReturnsCheckpoint proves a synced type with no post-checkpoint log
// returns exactly the checkpoint set (the common steady state right after a re-anchor + trim).
func TestRedisTypeSplicer_EmptyLogReturnsCheckpoint(t *testing.T) {
	splicer, snap, _ := spliceFixture(t)
	ctx := context.Background()

	require.NoError(t, snap.ReplaceTypeObjects(ctx, "", "v1", "configmaps", map[string]string{
		"default/a": checkpointEnvelope("a"),
	}, "100"))

	objs, rv, hc, err := splicer.SpliceType(ctx, "", "configmaps")
	require.NoError(t, err)
	assert.Equal(t, "100", rv)
	assert.Equal(t, "100-18446744073709551615", hc,
		"with no post-checkpoint log the coverage head is the top of the checkpoint rv (R-maxseq)")
	require.Len(t, objs, 1)
	assert.Equal(t, "a", objs[0].GetName())
}

// TestRedisTypeSplicer_SkipsUnconvertibleBodies proves a metav1.Status body in the log is dropped
// best-effort (the checkpoint backstops it), not folded into desired as a bogus object.
func TestRedisTypeSplicer_SkipsUnconvertibleBodies(t *testing.T) {
	splicer, snap, q := spliceFixture(t)
	ctx := context.Background()

	require.NoError(t, snap.ReplaceTypeObjects(ctx, "", "v1", "configmaps", map[string]string{
		"default/a": checkpointEnvelope("a"),
	}, "100"))

	statusEvent := configMapAuditEvent("update", "a", "150", "v2")
	statusEvent.ResponseObject = &runtime.Unknown{
		Raw: []byte(`{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Conflict"}`),
	}
	require.NoError(t, q.Enqueue(ctx, statusEvent))

	objs, _, hc, err := splicer.SpliceType(ctx, "", "configmaps")
	require.NoError(t, err)
	// The Status body carries no resourceVersion, so it ingests rv-less; with the main stream still
	// empty it diverts to the late lane rather than the main stream. The main stream stays empty, so
	// the coverage head holds at the top of the checkpoint rv — the entry the tail will never replay.
	assert.Equal(t, "100-18446744073709551615", hc,
		"an rv-less body diverts to the late lane; the coverage head stays at the checkpoint top")
	require.Len(t, objs, 1, "the Status body is not folded as an object")
	assert.Equal(t, "v1", configMapData(t, objs[0].Object), "a keeps its checkpoint state")
}
