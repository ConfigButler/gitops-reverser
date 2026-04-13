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

package e2e

import "testing"

func TestRemoveGpgsigHeader_PreservesTrailingNewlineState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "commit object without trailing newline",
			raw: "tree deadbeef\n" +
				"author Test <test@example.com> 1 +0000\n" +
				"committer Test <test@example.com> 1 +0000\n" +
				"gpgsig -----BEGIN SSH SIGNATURE-----\n" +
				" AAAA\n" +
				" -----END SSH SIGNATURE-----\n" +
				"\n" +
				"signed commit",
			want: "tree deadbeef\n" +
				"author Test <test@example.com> 1 +0000\n" +
				"committer Test <test@example.com> 1 +0000\n" +
				"\n" +
				"signed commit",
		},
		{
			name: "commit object with trailing newline",
			raw: "tree deadbeef\n" +
				"author Test <test@example.com> 1 +0000\n" +
				"committer Test <test@example.com> 1 +0000\n" +
				"gpgsig -----BEGIN SSH SIGNATURE-----\n" +
				" AAAA\n" +
				" -----END SSH SIGNATURE-----\n" +
				"\n" +
				"signed commit\n",
			want: "tree deadbeef\n" +
				"author Test <test@example.com> 1 +0000\n" +
				"committer Test <test@example.com> 1 +0000\n" +
				"\n" +
				"signed commit\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := removeGpgsigHeader(tt.raw); got != tt.want {
				t.Fatalf("removeGpgsigHeader() mismatch\nwant: %q\ngot:  %q", tt.want, got)
			}
		})
	}
}
