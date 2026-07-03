// SPDX-License-Identifier: Apache-2.0

package labserver

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseGVRs(t *testing.T) {
	got, err := ParseGVRs(" v1/configmaps , apps/v1/deployments ,")
	if err != nil {
		t.Fatalf("ParseGVRs: %v", err)
	}
	want := []schema.GroupVersionResource{
		{Version: "v1", Resource: "configmaps"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d gvrs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("gvr[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseGVRs_Invalid(t *testing.T) {
	if _, err := ParseGVRs("configmaps"); err == nil {
		t.Fatal("expected error for single-part resource")
	}
}

func TestParseGVRs_Empty(t *testing.T) {
	got, err := ParseGVRs("   ,  ")
	if err != nil || len(got) != 0 {
		t.Fatalf("empty spec: got %+v err %v", got, err)
	}
}
