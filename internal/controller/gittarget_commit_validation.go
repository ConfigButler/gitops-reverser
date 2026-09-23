// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	gitpkg "github.com/ConfigButler/gitops-reverser/internal/git"
)

// validateCommitConfig checks a GitTarget's spec.commit, returning ok=false and an
// operator-facing message when it cannot be honoured as written.
//
// The templates were validated on the GitProvider until this release, and the check moves with the
// field rather than being dropped: a template that fails to render is a mistake whose only other
// symptom is a commit that never happens, discovered from a log line. Both halves are checked
// against the SAME rendering path the write path uses, so admission and the writer cannot disagree
// about what a template means.
//
// The window is NOT checked here. It is a metav1.Duration behind the same duration pattern every
// other time field in this API carries, so the API server rejects a malformed or negative value at
// admission, with a message naming the field. A second check here could only repeat it.
func validateCommitConfig(target *configbutleraiv1alpha3.GitTarget) (bool, string) {
	if target.Spec.Commit == nil {
		return true, ""
	}

	if message := target.Spec.Commit.Message; message != nil {
		if message.EventTemplate != "" || message.GroupTemplate != "" {
			return false, "spec.commit.message.eventTemplate and groupTemplate are retired; migrate to liveTemplate"
		}
	}

	config := gitpkg.ResolveCommitConfig(nil).WithTargetMessage(target.Spec.Commit.Message)
	if err := gitpkg.ValidateCommitConfig(config); err != nil {
		return false, fmt.Sprintf("invalid spec.commit.message: %v", err)
	}
	return true, ""
}
