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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// recordingObjectMirror captures the calls mirrorTypeObjects / clearTypeObjects make.
type recordingObjectMirror struct {
	replacedItems   map[string]string
	replacedRV      string
	replacedKey     string
	replacedVersion string
	replaceCount    int
	deletedKey      string
}

func (r *recordingObjectMirror) ReplaceTypeObjects(
	_ context.Context, group, version, resource string, items map[string]string, rv string,
) error {
	r.replacedKey = group + "/" + resource
	r.replacedVersion = version
	r.replacedItems = items
	r.replacedRV = rv
	r.replaceCount++
	return nil
}

func (r *recordingObjectMirror) DeleteTypeObjects(_ context.Context, group, resource string) error {
	r.deletedKey = group + "/" + resource
	return nil
}

func TestMirrorTypeObjects_ListsAndReplaces(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(),
		streamedCM("default", "a", "10"),
		streamedCM("kube-system", "b", "11"),
	)
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), dynamicClient: dc, ObjectMirror: mirror}

	m.mirrorTypeObjects(context.Background(), logr.Discard(), configMapGVR)

	assert.Equal(t, "/configmaps", mirror.replacedKey, "core group + resource identify the type")
	require.Len(t, mirror.replacedItems, 2)
	assert.Contains(t, mirror.replacedItems, "default/a")
	assert.Contains(t, mirror.replacedItems, "kube-system/b")
	assert.NotEmpty(t, mirror.replacedRV, "the list resourceVersion pins the snapshot")

	// Each item is an envelope: identity + rv lifted out beside the sanitized body.
	var env objectEnvelope
	require.NoError(t, json.Unmarshal([]byte(mirror.replacedItems["default/a"]), &env))
	assert.Equal(t, "configmaps", env.Resource)
	assert.Equal(t, "v1", env.APIVersion)
	assert.Equal(t, "ConfigMap", env.Kind)
	assert.Equal(t, "default", env.Namespace)
	assert.Equal(t, "a", env.Name)
	assert.Equal(t, "10", env.ResourceVersion, "rv is lifted out of the body (sanitize strips it)")
	assert.Contains(t, string(env.Object), "ConfigMap", "the sanitized object rides along under object")
}

func TestMirrorTypeObjects_NilMirrorIsNoOp(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), streamedCM("default", "a", "10"))
	m := &Manager{Log: logr.Discard(), dynamicClient: dc} // ObjectMirror nil

	assert.NotPanics(t, func() {
		m.mirrorTypeObjects(context.Background(), logr.Discard(), configMapGVR)
	})
}

func TestClearTypeObjects_Deletes(t *testing.T) {
	mirror := &recordingObjectMirror{}
	m := &Manager{Log: logr.Discard(), ObjectMirror: mirror}

	m.clearTypeObjects(context.Background(), logr.Discard(), configMapGVR)

	assert.Equal(t, "/configmaps", mirror.deletedKey)
}
