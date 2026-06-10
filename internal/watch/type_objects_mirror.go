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

package watch

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
)

// objectEnvelope is the per-object value stored in "<base>:objects:items". It lifts the
// identity, resourceVersion, and generation out of the body — sanitize strips exactly those
// server fields, so they would otherwise be unreadable — and stores them beside the sanitized
// object under the same field names the audit stream overview uses, keeping the two
// structures directly joinable.
type objectEnvelope struct {
	APIGroup        string          `json:"api_group"`
	APIVersion      string          `json:"api_version"`
	Resource        string          `json:"resource"`
	Kind            string          `json:"kind,omitempty"`
	Namespace       string          `json:"namespace,omitempty"`
	Name            string          `json:"name"`
	UID             string          `json:"uid,omitempty"`
	ResourceVersion string          `json:"resource_version,omitempty"`
	Generation      int64           `json:"generation,omitempty"`
	Object          json.RawMessage `json:"object"`
}

// mirrorTypeObjects lists the current set of objects for a type and replaces the type's
// checkpoint in the per-resource-type keyspace, returning the list's resourceVersion — the rv
// the checkpoint is pinned to, which the Materializer records as the type's Synced revision.
// It is the demand-driven checkpoint LIST the materialization driver runs for a CLAIMED type
// (L-3, runTypeCheckpointSync), no longer unconditionally on every activation. A nil mirror or
// missing dynamic client is a benign no-op (empty rv, no error); a list or replace error is
// returned so the driver records SyncFailed and the prior checkpoint (if any) keeps serving.
// See docs/design/stream/per-resource-type-rv-keyed-streams-experiment.md.
func (m *Manager) mirrorTypeObjects(
	ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource,
) (string, error) {
	if m.ObjectMirror == nil {
		return "", nil
	}
	dc := m.dynamicClientFromConfig(log)
	if dc == nil {
		return "", nil
	}

	// Empty namespace lists across all namespaces (and is correct for cluster-scoped types).
	list, err := dc.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("objects-mirror: list %s: %w", gvr.String(), err)
	}

	items := make(map[string]string, len(list.Items))
	for i := range list.Items {
		obj := &list.Items[i]
		raw, err := objectEnvelopeJSON(gvr, obj)
		if err != nil {
			log.Error(err, "objects-mirror: marshal failed",
				"gvr", gvr.String(), "object", objectIdentity(obj))
			continue
		}
		items[objectIdentity(obj)] = raw
	}

	rv := list.GetResourceVersion()
	if err := m.ObjectMirror.ReplaceTypeObjects(ctx, gvr.Group, gvr.Version, gvr.Resource, items, rv); err != nil {
		return "", fmt.Errorf("objects-mirror: replace %s: %w", gvr.String(), err)
	}
	log.Info("objects-mirror: snapshot loaded", "gvr", gvr.String(), "count", len(items), "resourceVersion", rv)
	return rv, nil
}

// clearTypeObjects drops a removed type's stored object snapshot. Best-effort like the load.
func (m *Manager) clearTypeObjects(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	if m.ObjectMirror == nil {
		return
	}
	if err := m.ObjectMirror.DeleteTypeObjects(ctx, gvr.Group, gvr.Resource); err != nil {
		log.Error(err, "objects-mirror: delete failed", "gvr", gvr.String())
		return
	}
	log.Info("objects-mirror: snapshot cleared", "gvr", gvr.String())
}

// objectEnvelopeJSON builds the stored value for one object: its identity, resourceVersion,
// and generation (read from the original object, since sanitize strips them) wrapped around
// the sanitized body. The GVR supplies the group/version/resource so each entry is
// self-describing without consulting its key.
func objectEnvelopeJSON(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (string, error) {
	body, err := sanitize.Sanitize(obj).MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("marshal sanitized object: %w", err)
	}
	raw, err := json.Marshal(objectEnvelope{
		APIGroup:        gvr.Group,
		APIVersion:      gvr.Version,
		Resource:        gvr.Resource,
		Kind:            obj.GetKind(),
		Namespace:       obj.GetNamespace(),
		Name:            obj.GetName(),
		UID:             string(obj.GetUID()),
		ResourceVersion: obj.GetResourceVersion(),
		Generation:      obj.GetGeneration(),
		Object:          body,
	})
	if err != nil {
		return "", fmt.Errorf("marshal object envelope: %w", err)
	}
	return string(raw), nil
}

// objectIdentity is the per-object hash field: "<namespace>/<name>" for namespaced objects,
// or just "<name>" for cluster-scoped ones.
func objectIdentity(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" {
		return ns + "/" + obj.GetName()
	}
	return obj.GetName()
}
