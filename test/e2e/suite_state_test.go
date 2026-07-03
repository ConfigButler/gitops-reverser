// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strings"
	"testing"
)

func TestMarkSuiteWidePreservation_DeduplicatesReasons(t *testing.T) {
	resetE2EPreservationStateForTest()

	markSuiteWidePreservation("first reason")
	markSuiteWidePreservation("first reason")
	markSuiteWidePreservation("  ")

	if !suiteWidePreserve.Load() {
		t.Fatal("expected suite-wide preservation to be enabled")
	}

	summary := e2ePreservationSummary()
	if !strings.Contains(summary, "first reason") {
		t.Fatalf("expected summary to include first reason: %q", summary)
	}
	if !strings.Contains(summary, "preserve requested") {
		t.Fatalf("expected summary to include default reason: %q", summary)
	}
	if strings.Count(summary, "first reason") != 1 {
		t.Fatalf("expected duplicate reasons to be collapsed: %q", summary)
	}
}

func TestPreserveNamespace_TracksNamespaceScope(t *testing.T) {
	resetE2EPreservationStateForTest()

	preserveNamespace(" scoped-ns ")

	if !isPreservedNamespace("scoped-ns") {
		t.Fatal("expected preserved namespace to be tracked")
	}
	if isPreservedNamespace("other-ns") {
		t.Fatal("did not expect unrelated namespace to be preserved")
	}
}

func resetE2EPreservationStateForTest() {
	suiteWidePreserve.Store(false)

	suiteWidePreserveMu.Lock()
	suiteWidePreserveReasons = nil
	suiteWidePreserveMu.Unlock()

	preservedNamespacesMu.Lock()
	preservedNamespaces = map[string]struct{}{}
	preservedNamespacesMu.Unlock()
}
