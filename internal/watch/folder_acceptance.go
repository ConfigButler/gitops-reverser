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

// MarkTargetFolderRefused records that the GitTarget folder failed the structure-only
// acceptance gate. The refusal is target-wide, not stream-specific.
func (m *Manager) MarkTargetFolderRefused(gitDest types.ResourceReference, reason, message string) {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	if m.targetFolderAcceptance == nil {
		m.targetFolderAcceptance = map[string]FolderAcceptanceStatus{}
	}
	m.targetFolderAcceptance[gitDest.Key()] = FolderAcceptanceStatus{
		Accepted: false,
		Reason:   reason,
		Message:  message,
		At:       metav1.Now(),
	}
}

// MarkTargetFolderAccepted clears any prior refusal for the GitTarget folder.
func (m *Manager) MarkTargetFolderAccepted(gitDest types.ResourceReference) {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	if m.targetFolderAcceptance == nil {
		m.targetFolderAcceptance = map[string]FolderAcceptanceStatus{}
	}
	m.targetFolderAcceptance[gitDest.Key()] = FolderAcceptanceStatus{
		Accepted: true,
		Reason:   "FolderAccepted",
		Message:  "GitTarget folder accepted",
		At:       metav1.Now(),
	}
}

// FolderAcceptanceForGitTarget returns the latest acceptance status for the GitTarget.
// Missing state means no refusal has been observed, so the folder is accepted.
func (m *Manager) FolderAcceptanceForGitTarget(gitDest types.ResourceReference) FolderAcceptanceStatus {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	if m.targetFolderAcceptance != nil {
		if st, ok := m.targetFolderAcceptance[gitDest.Key()]; ok {
			return st
		}
	}
	return FolderAcceptanceStatus{
		Accepted: true,
		Reason:   "FolderAccepted",
		Message:  "GitTarget folder accepted",
	}
}

func (m *Manager) dropTargetFolderAcceptanceLocked(gitDest types.ResourceReference) {
	if m.targetFolderAcceptance != nil {
		delete(m.targetFolderAcceptance, gitDest.Key())
	}
}
