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
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func cmUnstructured(ns, name, data string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"data":       map[string]any{"k": data},
	}}
}

// TestCompareDesiredSets covers the four buckets: agree (same body), mismatch (same identity,
// different body), audit-only, and watch-only.
func TestCompareDesiredSets(t *testing.T) {
	audit := []*unstructured.Unstructured{
		cmUnstructured("default", "a", "v2"), // agrees with watch
		cmUnstructured("default", "b", "v1"), // mismatches watch's b
		cmUnstructured("default", "d", "vd"), // audit-only
	}
	watch := []*unstructured.Unstructured{
		cmUnstructured("default", "a", "v2"), // agrees
		cmUnstructured("default", "b", "v9"), // different body -> mismatch
		cmUnstructured("default", "c", "vc"), // watch-only
	}

	div := compareDesiredSets(audit, watch)
	assert.Equal(t, 3, div.AuditCount)
	assert.Equal(t, 3, div.WatchCount)
	assert.Equal(t, 1, div.Agree, "a agrees")
	assert.Equal(t, []string{"default/b"}, div.Mismatch)
	assert.Equal(t, []string{"default/d"}, div.AuditOnly)
	assert.Equal(t, []string{"default/c"}, div.WatchOnly)
	assert.True(t, div.diverged())
}

// TestCompareDesiredSets_IdenticalAgree proves two identical sets diverge in no way.
func TestCompareDesiredSets_IdenticalAgree(t *testing.T) {
	set := func() []*unstructured.Unstructured {
		return []*unstructured.Unstructured{
			cmUnstructured("default", "a", "v1"),
			cmUnstructured("ns2", "b", "v2"),
		}
	}
	div := compareDesiredSets(set(), set())
	assert.False(t, div.diverged())
	assert.Equal(t, 2, div.Agree)
	assert.Empty(t, div.AuditOnly)
	assert.Empty(t, div.WatchOnly)
	assert.Empty(t, div.Mismatch)
}

type fakeAuditSplicer struct {
	objs []*unstructured.Unstructured
	err  error
}

func (f fakeAuditSplicer) SpliceType(
	_ context.Context, _, _ string,
) ([]*unstructured.Unstructured, string, string, error) {
	return f.objs, "100", "100-0", f.err
}

type fakeWatchSplicer struct {
	objs []*unstructured.Unstructured
	err  error
}

func (f fakeWatchSplicer) SpliceWatchType(
	_ context.Context, _, _ string,
) ([]*unstructured.Unstructured, string, error) {
	return f.objs, "100", f.err
}

// TestComputeWatchAuditDivergence wires fake splices onto the Manager and proves it diffs their two
// sets, and that an error from either splice skips the type (ok=false) rather than reporting a
// spurious divergence.
func TestComputeWatchAuditDivergence(t *testing.T) {
	ctx := context.Background()

	m := &Manager{
		Log: logr.Discard(),
		TypeSplicer: fakeAuditSplicer{
			objs: []*unstructured.Unstructured{
				cmUnstructured("default", "a", "v1"),
				cmUnstructured("default", "b", "v1"),
			},
		},
		WatchStateSplicer: fakeWatchSplicer{
			objs: []*unstructured.Unstructured{
				cmUnstructured("default", "a", "v1"),
				cmUnstructured("default", "c", "vc"),
			},
		},
	}
	div, ok := m.computeWatchAuditDivergence(ctx, configmapsGVR)
	require.True(t, ok)
	assert.Equal(t, 1, div.Agree, "a agrees")
	assert.Equal(t, []string{"default/b"}, div.AuditOnly, "b missing from the watch set")
	assert.Equal(t, []string{"default/c"}, div.WatchOnly, "c only in the watch set")

	// A splice error skips the type.
	mErr := &Manager{
		Log:               logr.Discard(),
		TypeSplicer:       fakeAuditSplicer{err: errors.New("no checkpoint")},
		WatchStateSplicer: fakeWatchSplicer{},
	}
	_, ok = mErr.computeWatchAuditDivergence(ctx, configmapsGVR)
	assert.False(t, ok, "an audit-splice error skips the comparison")

	mWErr := &Manager{
		Log:               logr.Discard(),
		TypeSplicer:       fakeAuditSplicer{},
		WatchStateSplicer: fakeWatchSplicer{err: errors.New("boom")},
	}
	_, ok = mWErr.computeWatchAuditDivergence(ctx, configmapsGVR)
	assert.False(t, ok, "a watch-splice error skips the comparison")
}

// TestSampleIdentities proves the log sample is bounded.
func TestSampleIdentities(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, sampleIdentities([]string{"a", "b"}))
	assert.Len(t, sampleIdentities([]string{"a", "b", "c", "d", "e", "f", "g"}), 5)
}
