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

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// DeclareForGitTarget ensures the GitTarget's watch-first data plane is running.
func (m *Manager) DeclareForGitTarget(
	ctx context.Context,
	gitDest types.ResourceReference,
	forceRecheck ...bool,
) error {
	// Capture the UID before starting watches: the data plane keys its resume cursors
	// by GitTarget UID, which the rule-derived watch tables do not carry.
	m.rememberGitTargetUID(gitDest)
	force := len(forceRecheck) > 0 && forceRecheck[0]
	if err := m.EnsureGitTargetWatches(ctx, gitDest, force); err != nil {
		m.Log.Info("watch-first declare skipped; surface not observable",
			"gitDest", gitDest.String(), "err", err.Error())
		return err
	}
	return nil
}

// ForgetGitTargetDeclaration drops in-memory watch state for a deleted GitTarget.
func (m *Manager) ForgetGitTargetDeclaration(gitDest types.ResourceReference) {
	m.forgetGitTargetWatches(gitDest)
	m.forgetGitTargetUID(gitDest)
	m.declaredGVRsMu.Lock()
	defer m.declaredGVRsMu.Unlock()
	delete(m.declaredGVRs, gitDest.String())
}
