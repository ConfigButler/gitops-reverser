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
