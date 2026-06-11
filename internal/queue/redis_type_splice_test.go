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

	objs, rv, err := splicer.SpliceType(ctx, "", "configmaps")
	require.NoError(t, err)
	assert.Equal(t, "100", rv, "the splice is anchored at the checkpoint revision")

	require.Len(t, objs, 2, "b was deleted; a and c remain")
	assert.Equal(t, "default/a", objs[0].GetNamespace()+"/"+objs[0].GetName(), "sorted by identity")
	assert.Equal(t, "default/c", objs[1].GetNamespace()+"/"+objs[1].GetName())
	assert.Equal(t, "v2", configMapData(t, objs[0].Object), "a reflects the post-checkpoint update")
	assert.Equal(t, "vc", configMapData(t, objs[1].Object), "c was created from the log")
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

	objs, rv, err := splicer.SpliceType(ctx, "apps", "deployments")
	require.NoError(t, err)
	assert.Equal(t, "100", rv)

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

	_, _, err := splicer.SpliceType(ctx, "", "configmaps")
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

	objs, rv, err := splicer.SpliceType(ctx, "", "configmaps")
	require.NoError(t, err)
	assert.Equal(t, "100", rv)
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

	objs, _, err := splicer.SpliceType(ctx, "", "configmaps")
	require.NoError(t, err)
	require.Len(t, objs, 1, "the Status body is not folded as an object")
	assert.Equal(t, "v1", configMapData(t, objs[0].Object), "a keeps its checkpoint state")
}
