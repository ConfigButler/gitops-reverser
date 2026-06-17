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

package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

type fakeMirrorGate struct{ allowed map[string]bool }

func (f fakeMirrorGate) Allow(group, resource string) bool {
	return f.allowed[group+"/"+resource]
}

func gatedEvent(id, group, resource string) *auditv1.Event {
	return &auditv1.Event{
		AuditID:   types.UID(id),
		ObjectRef: &auditv1.ObjectReference{APIGroup: group, Resource: resource},
	}
}

// TestMirrorByType_DemandGated proves the gate is consulted on the mirror hot path: only events for
// a type currently in the required-set reach the queue; everything else is skipped. This is the
// wiring counterpart to the gate component's own tests (internal/gate).
func TestMirrorByType_DemandGated(t *testing.T) {
	q := &recordingAuditEventQueue{}
	gate := fakeMirrorGate{allowed: map[string]bool{"apps/deployments": true}}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: q, MirrorGate: gate})
	require.NoError(t, err)

	handler.mirrorByType(context.Background(), gatedEvent("wanted", "apps", "deployments"))
	handler.mirrorByType(context.Background(), gatedEvent("unwanted", "apps", "statefulsets"))
	handler.mirrorByType(context.Background(), &auditv1.Event{AuditID: types.UID("noref")}) // __unknown__, never wanted

	assert.Equal(t, []string{"wanted"}, q.auditIDs(),
		"only the required type is mirrored; unwanted types and ref-less events are skipped")
}

// TestMirrorByType_NilGateMirrorsEverything keeps the pre-gate behaviour: a nil gate disables
// gating, so every accepted event is still mirrored (no regression for existing deployments).
func TestMirrorByType_NilGateMirrorsEverything(t *testing.T) {
	q := &recordingAuditEventQueue{}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: q})
	require.NoError(t, err)

	handler.mirrorByType(context.Background(), gatedEvent("x", "apps", "statefulsets"))

	assert.Equal(t, []string{"x"}, q.auditIDs(), "nil gate mirrors everything (legacy behaviour)")
}
