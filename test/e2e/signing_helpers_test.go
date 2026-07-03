// SPDX-License-Identifier: Apache-2.0

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
