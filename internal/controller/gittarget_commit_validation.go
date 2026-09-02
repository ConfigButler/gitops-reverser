// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"

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
// The window is checked here too, even though the write path already falls back to the default on
// an unparseable value. The fallback exists so a stored mistake cannot stop a target mirroring; the
// check exists so a new one is visible before it silently changes the commit cadence.
func validateCommitConfig(target *configbutleraiv1alpha3.GitTarget) (bool, string) {
	if target.Spec.Commit == nil {
		return true, ""
	}

	if window := target.Spec.Commit.Window; window != nil {
		parsed, err := time.ParseDuration(*window)
		if err != nil {
			return false, fmt.Sprintf(
				"spec.commit.window %q is not a duration: %v; use a Go duration such as \"5s\" or \"0s\"",
				*window, err)
		}
		if parsed < 0 {
			return false, fmt.Sprintf(
				"spec.commit.window %q is negative; use \"0s\" to commit once per event", *window)
		}
	}

	config := gitpkg.ResolveCommitConfig(nil).WithTargetMessage(target.Spec.Commit.Message)
	if err := gitpkg.ValidateCommitConfig(config); err != nil {
		return false, fmt.Sprintf("invalid spec.commit.message: %v", err)
	}
	return true, ""
}
