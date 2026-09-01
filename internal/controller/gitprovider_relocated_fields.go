// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// ErrRelocatedCommitFields is the refusal for a STORED GitProvider that still carries the two
// fields this release moved onto GitTarget.
//
// Admission rejects them on write, but an object written by an earlier release keeps them in etcd,
// and the two ways of carrying on are both silent: honouring them would keep a folder's commit
// cadence and message coming from the connection after the API says they come from the folder, and
// ignoring them would change both without saying so. Refusing is the third option, and the only one
// an operator can see.
var ErrRelocatedCommitFields = errors.New(
	"spec.push and spec.commit.message describe the FOLDER being written, not this connection, and " +
		"have moved to GitTarget.spec.commit.window and GitTarget.spec.commit.message. This " +
		"GitProvider still carries at least one of them, so it is refused rather than silently " +
		"reinterpreted: set the values on each GitTarget that needs them, then remove them here")

// refuseRelocatedCommitFields reports the stored-field refusal, or nil when the provider carries
// neither field.
func refuseRelocatedCommitFields(gitProvider *configbutleraiv1alpha3.GitProvider) error {
	//nolint:staticcheck // reading the deprecated fields is the point: they must be refused, not pruned.
	if gitProvider.Spec.Push != nil {
		return ErrRelocatedCommitFields
	}
	//nolint:staticcheck // reading the deprecated field is the point: it must be refused, not pruned.
	if gitProvider.Spec.Commit != nil && gitProvider.Spec.Commit.Message != nil {
		return ErrRelocatedCommitFields
	}
	return nil
}
