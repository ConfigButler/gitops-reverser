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

import "testing"

func TestShouldIgnoreResource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		group    string
		resource string
		want     bool
	}{
		{name: "core secrets", group: "", resource: "secrets", want: true},
		{name: "core secrets case insensitive", group: "", resource: "Secrets", want: true},
		{name: "core configmaps", group: "", resource: "configmaps", want: false},
		{name: "non-core secrets", group: "example.com", resource: "secrets", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shouldIgnoreResource(tt.group, tt.resource)
			if got != tt.want {
				t.Fatalf("shouldIgnoreResource(%q, %q) = %v, want %v", tt.group, tt.resource, got, tt.want)
			}
		})
	}
}
