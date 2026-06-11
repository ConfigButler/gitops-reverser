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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// The transitions that carry no git action (Wobbling/Recovered/Refused) are inert in the
// handler: they neither reconcile nor sweep, so a Manager with no EventRouter handles them
// without touching the (nil) router.
func TestHandleTypeLifecycleEvent_NoActionTransitionsAreInert(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	for _, kind := range []typeset.EventKind{typeset.TypeWobbling, typeset.TypeRecovered, typeset.TypeRefused} {
		// Must not panic or dereference the nil EventRouter for a no-git-action transition.
		assert.NotPanics(t, func() {
			m.handleTypeLifecycleEvent(ctx, logr.Discard(), typeset.LifecycleEvent{Kind: kind, GVR: gvr})
		})
	}
}
