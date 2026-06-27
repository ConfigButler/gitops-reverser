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

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// MarkTargetGitPathRefused records that the GitTarget path failed the structure-only
// acceptance gate. The refusal is target-wide, not stream-specific.
func (m *Manager) MarkTargetGitPathRefused(gitDest types.ResourceReference, reason, message string) {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	if m.targetGitPathAcceptance == nil {
		m.targetGitPathAcceptance = map[string]GitPathAcceptanceStatus{}
	}
	m.targetGitPathAcceptance[gitDest.Key()] = GitPathAcceptanceStatus{
		Accepted: false,
		Reason:   reason,
		Message:  message,
		At:       metav1.Now(),
	}
}

// MarkTargetGitPathAccepted clears any prior refusal for the GitTarget path.
func (m *Manager) MarkTargetGitPathAccepted(gitDest types.ResourceReference) {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	if m.targetGitPathAcceptance == nil {
		m.targetGitPathAcceptance = map[string]GitPathAcceptanceStatus{}
	}
	m.targetGitPathAcceptance[gitDest.Key()] = GitPathAcceptanceStatus{
		Accepted: true,
		Reason:   "GitPathAccepted",
		Message:  "GitTarget path accepted",
		At:       metav1.Now(),
	}
}

// GitPathAcceptanceForGitTarget returns the latest acceptance status for the GitTarget.
// Missing state means no refusal has been observed, so the path is accepted.
func (m *Manager) GitPathAcceptanceForGitTarget(gitDest types.ResourceReference) GitPathAcceptanceStatus {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	if m.targetGitPathAcceptance != nil {
		if st, ok := m.targetGitPathAcceptance[gitDest.Key()]; ok {
			return st
		}
	}
	return GitPathAcceptanceStatus{
		Accepted: true,
		Reason:   "GitPathAccepted",
		Message:  "GitTarget path accepted",
	}
}

func (m *Manager) dropTargetGitPathAcceptanceLocked(gitDest types.ResourceReference) {
	if m.targetGitPathAcceptance != nil {
		delete(m.targetGitPathAcceptance, gitDest.Key())
	}
}
