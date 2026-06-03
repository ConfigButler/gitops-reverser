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

package manifestreport

// This test guards a BuildReport defect fixed on this branch. It started red (as
// the executable spec for the fix) and now pins the corrected behavior.
//
//   Medium: BuildReport must not misclassify a desired resource whose existing
//   Git document is non-editable. Inventory.Location only contains editable
//   records (non-editable ones are skipped while indexing), so a desired object
//   whose only Git copy is an anchor/alias/etc. used to be reported as
//   ActionCreate (no location found) AND, separately, the same identity's
//   non-editable record as ActionSkip — two contradictory verdicts. It must be a
//   single skip.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A resource that exists in Git as a non-editable document (here an anchor/alias)
// AND still exists in the cluster must produce exactly one verdict — a skip —
// never a contradictory ActionCreate + ActionSkip pair.
func TestBuildReport_NonEditableDesiredIsNotDoubleClassified(t *testing.T) {
	anchor := manifestedit.FileContent{Path: "anchor.yaml", Content: []byte(
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: anc\n  namespace: default\n" +
			"data:\n  a: &x 1\n  b: *x\n")}

	// The cluster still has this exact resource (same identity as the Git doc).
	desired := configMap("anc", "blue")

	report, _ := BuildReport(
		[]manifestedit.FileContent{anchor},
		[]*unstructured.Unstructured{desired},
	)

	var actions []Action
	for _, e := range report.Entries {
		if e.Identity.Name == "anc" {
			actions = append(actions, e.Action)
		}
	}

	require.Lenf(t, actions, 1,
		"one identity present in both Git and cluster must yield one verdict, got %v", actions)
	assert.Equal(t, ActionSkip, actions[0],
		"a non-editable Git document is a skip, never a create")
}
