// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestScanEntries_ACommitRequestIsNeverMirrored pins that a CommitRequest is not state to mirror: a
// WatchRule that names its type, or covers it with "*", finds it denied by policy, so a request kept
// in a folder beside the objects it saves never reaches Git.
func TestScanEntries_ACommitRequestIsNeverMirrored(t *testing.T) {
	entries := scanEntriesForResourceList(&metav1.APIResourceList{
		GroupVersion: "configbutler.ai/v1alpha3",
		APIResources: []metav1.APIResource{
			{Name: "commitrequests", Kind: "CommitRequest", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
			{Name: "commitrequests/status", Kind: "CommitRequest", Namespaced: true, Verbs: []string{"get"}},
			{Name: "watchrules", Kind: "WatchRule", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
		},
	}, true)

	allowed := map[string]bool{}
	for _, entry := range entries {
		allowed[entry.GVR.Resource] = entry.Allowed
		if entry.GVR.Resource == "commitrequests" {
			assert.Equal(t, defaultResourceExclusionReason, entry.PolicyReason)
		}
	}
	assert.False(t, allowed["commitrequests"], "a CommitRequest is a request, never mirrored")
	assert.True(t, allowed["watchrules"], "the other configbutler.ai kinds are not excluded by this rule")
}
