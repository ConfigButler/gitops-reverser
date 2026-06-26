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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

// fakeLookup returns fact/ok after `hitAfter` calls; calls counts invocations.
type fakeLookup struct {
	fact     queue.AuthorFact
	ok       bool
	hitAfter int
	calls    int
}

func (f *fakeLookup) LookupAuthor(
	_ context.Context, _ schema.GroupVersionResource, _, _ string, _ k8stypes.UID, _ string,
) (queue.AuthorFact, bool) {
	f.calls++
	if f.calls >= f.hitAfter {
		return f.fact, f.ok
	}
	return queue.AuthorFact{}, false
}

var resolverGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

func TestAuthorResolver_HumanHit(t *testing.T) {
	lookup := &fakeLookup{fact: queue.AuthorFact{Author: "alice", Email: "a@x.io"}, ok: true, hitAfter: 1}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, SANamePolicyName, logr.Discard())

	ui, ok := r.ResolveAuthor(context.Background(), resolverGVR, "team-a", "web", "uid-1", "101")
	require.True(t, ok)
	assert.Equal(t, "alice", ui.Username)
	assert.Equal(t, "a@x.io", ui.Email)
	assert.Equal(t, 1, lookup.calls)
}

func TestAuthorResolver_ServiceAccountNamePolicy(t *testing.T) {
	sa := "system:serviceaccount:flux-system:kustomize-controller"
	lookup := &fakeLookup{fact: queue.AuthorFact{Author: sa, IsServiceAccount: true}, ok: true, hitAfter: 1}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, SANamePolicyName, logr.Discard())

	ui, ok := r.ResolveAuthor(context.Background(), resolverGVR, "team-a", "web", "uid-1", "101")
	require.True(t, ok)
	assert.Equal(t, sa, ui.Username)
}

func TestAuthorResolver_ServiceAccountBotPolicyCollapsesToCommitter(t *testing.T) {
	sa := "system:serviceaccount:flux-system:kustomize-controller"
	lookup := &fakeLookup{fact: queue.AuthorFact{Author: sa, IsServiceAccount: true}, ok: true, hitAfter: 1}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, SANamePolicyBot, logr.Discard())

	_, ok := r.ResolveAuthor(context.Background(), resolverGVR, "team-a", "web", "uid-1", "101")
	assert.False(t, ok, "a service account under the bot policy commits as the committer")
}

func TestAuthorResolver_MissExpiresToCommitter(t *testing.T) {
	lookup := &fakeLookup{ok: false, hitAfter: 1000}
	r := NewAuthorResolver(lookup, 0, SANamePolicyName, logr.Discard())

	_, ok := r.ResolveAuthor(context.Background(), resolverGVR, "team-a", "web", "uid-1", "101")
	assert.False(t, ok)
}

func TestAuthorResolver_WaitsThroughGraceWindowForLateFact(t *testing.T) {
	lookup := &fakeLookup{fact: queue.AuthorFact{Author: "bob"}, ok: true, hitAfter: 3}
	r := NewAuthorResolver(lookup, 2*time.Second, SANamePolicyName, logr.Discard())

	ui, ok := r.ResolveAuthor(context.Background(), resolverGVR, "team-a", "web", "uid-1", "101")
	require.True(t, ok)
	assert.Equal(t, "bob", ui.Username)
	assert.GreaterOrEqual(t, lookup.calls, 3)
}

func TestAuthorResolver_NilLookupIsCommitter(t *testing.T) {
	r := NewAuthorResolver(nil, DefaultAttributionGraceWindow, SANamePolicyName, logr.Discard())
	_, ok := r.ResolveAuthor(context.Background(), resolverGVR, "team-a", "web", "uid-1", "101")
	assert.False(t, ok)
}
