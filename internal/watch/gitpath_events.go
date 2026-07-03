// SPDX-License-Identifier: Apache-2.0

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
