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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// gitPathEventsBuffer sizes the acceptance-change channel. A full buffer means a reconcile is
// already pending for that GitTarget, so a dropped event is harmless — the periodic requeue is
// the backstop. The buffer just absorbs bursts without blocking the data plane.
const gitPathEventsBuffer = 256

// GitPathEvents returns the channel the GitTarget controller wires via source.Channel so a
// GitPath acceptance transition enqueues the owning GitTarget. It is lazily created so a
// zero-value Manager (tests) and the cmd-wired Manager share one channel.
func (m *Manager) GitPathEvents() <-chan event.GenericEvent {
	m.gitPathEventsMu.Lock()
	defer m.gitPathEventsMu.Unlock()
	if m.gitPathEventsCh == nil {
		m.gitPathEventsCh = make(chan event.GenericEvent, gitPathEventsBuffer)
	}
	return m.gitPathEventsCh
}

// enqueueGitPathChange emits a non-blocking GenericEvent for the GitTarget. The send is
// best-effort: if no controller has wired the channel yet, or the buffer is full, it is a
// no-op (a reconcile is already pending or will arrive via the periodic requeue).
func (m *Manager) enqueueGitPathChange(gitDest types.ResourceReference) {
	m.gitPathEventsMu.Lock()
	ch := m.gitPathEventsCh
	m.gitPathEventsMu.Unlock()
	if ch == nil {
		return
	}
	evt := event.GenericEvent{Object: &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: gitDest.Name, Namespace: gitDest.Namespace},
	}}
	select {
	case ch <- evt:
	default:
	}
}
