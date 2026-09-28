// SPDX-License-Identifier: Apache-2.0

package types

// OperationType classifies one observed change to an object: its creation, an update, or its
// deletion. It tags watch events and audit verbs alike, so both paths agree on what happened.
// It is not a selection switch: every selected resource collection is observed through its
// whole lifecycle, and the GitTarget's prune mode decides what a deletion does to Git.
type OperationType string

const (
	// OperationCreate tags an object's creation.
	OperationCreate OperationType = "CREATE"
	// OperationUpdate tags a change to an existing object.
	OperationUpdate OperationType = "UPDATE"
	// OperationDelete tags an object's deletion.
	OperationDelete OperationType = "DELETE"
)
