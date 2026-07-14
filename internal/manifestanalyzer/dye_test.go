// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// The dye's guardrails. Every one of these is a CORRECTNESS requirement rather than a
// preference, and every one was measured against kustomize rather than reasoned about.
// See docs/design/support-boundary/render-attribution.md §3 and §7.

// The charset is the one that will silently ruin a render if it is got wrong.
//
// kustomize does not validate a tag or a digest — anything renders through. But it MATCHES on
// the whole image string with a regex, so a nonce outside that charset leaves the image
// un-matchable and EVERY LATER ENTRY SILENTLY STOPS FIRING. No error, just a different render,
// and an attribution that credits the wrong entry.
func TestPlanDye_NoncesStayInsideKustomizesMatchCharset(t *testing.T) {
	// Exactly kustomize's own pattern (api/internal/image/image.go), for the image name "app".
	matcher := regexp.MustCompile(imageNamePattern("app"))

	plan := planDye([]manifestedit.FileContent{
		{Path: "kustomization.yaml", Content: []byte(
			"resources:\n  - d.yaml\nimages:\n" +
				"  - name: app\n    newTag: v1\n" +
				"  - name: app\n    digest: sha256:abc\n")},
	})
	require.NotEmpty(t, plan.byNonce)

	for nonce, mark := range plan.byNonce {
		var image string
		switch mark.field {
		case fieldNewTag:
			image = "app:" + nonce
		case fieldDigest:
			image = "app@" + nonce
		default:
			continue
		}
		require.True(t, matcher.MatchString(image),
			"the dyed image %q must still match kustomize's own pattern, or every later entry "+
				"silently stops firing", image)
	}
}

// A digest dye MUST carry the sha256: prefix. The pattern is `(@sha256:[...])?` — a bare nonce
// after the @ does not match, and the next entry goes quietly dead. Measured.
func TestPlanDye_DigestNonceCarriesTheMandatorySha256Prefix(t *testing.T) {
	plan := planDye([]manifestedit.FileContent{
		{Path: "kustomization.yaml", Content: []byte(
			"resources:\n  - d.yaml\nimages:\n  - name: app\n    digest: sha256:abc\n")},
	})

	found := false
	for nonce, mark := range plan.byNonce {
		if mark.field != fieldDigest {
			continue
		}
		found = true
		require.Regexp(t, `^sha256:[a-zA-Z0-9]+$`, nonce,
			"a digest dye without the sha256: prefix disables every later entry")
	}
	require.True(t, found, "the digest entry must have been dyed")
}

// Only a field the entry ALREADY DECLARES may be dyed. Injecting a newTag into a
// newName-only entry would fabricate a supplier that does not exist, and the projection would
// then route a tag edit onto an entry that never set a tag.
func TestPlanDye_OnlyDyesFieldsTheEntryDeclares(t *testing.T) {
	plan := planDye([]manifestedit.FileContent{
		{Path: "kustomization.yaml", Content: []byte(
			"resources:\n  - d.yaml\nimages:\n  - name: app\n    newName: mirror/app\n")},
	})

	for _, mark := range plan.byNonce {
		require.Equal(t, fieldNewName, mark.field,
			"the entry declares only newName, so only newName may carry a dye")
	}
}

// newName is NOT a pure sink — it is the join key for every later entry — so dyeing it can
// change which entries match and alter the render's shape. The guard is exact: a newName may be
// dyed unless some other entry's name: matches it.
func TestDyeingNamesIsSafe(t *testing.T) {
	tests := []struct {
		name    string
		entries []ImageOverride
		safe    bool
	}{
		{
			name: "no rename at all",
			entries: []ImageOverride{
				{Name: "app", NewTag: "v2", HasNewTag: true},
			},
			safe: true,
		},
		{
			name: "a rename nothing else refers to",
			entries: []ImageOverride{
				{Name: "app", NewName: "mirror/app", HasNewName: true},
				{Name: "other", NewTag: "v2", HasNewTag: true},
			},
			safe: true,
		},
		{
			name: "a rename CHAIN: a later entry keys off the new name",
			entries: []ImageOverride{
				{Name: "app", NewName: "mirror/app", HasNewName: true},
				{Name: "mirror/app", NewTag: "v2", HasNewTag: true},
			},
			safe: false,
		},
		{
			// The guard must be asked of kustomize's compiled pattern, not of string
			// equality: an entry's name is a REGULAR EXPRESSION, so `mirror/ap.` matches
			// `mirror/app` without being equal to it. A string-equality guard would dye the
			// name here, kill the second entry, and mis-attribute the tag.
			name: "a rename chain joined by a REGEX, not by string equality",
			entries: []ImageOverride{
				{Name: "app", NewName: "mirror/app", HasNewName: true},
				{Name: "mirror/ap.", NewTag: "v2", HasNewTag: true},
			},
			safe: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.safe, dyeingNamesIsSafe(tc.entries))
		})
	}
}

// The dye must be reproducible: the same tree mints the same nonces every time, or two renders
// of an unchanged repository would disagree and the store would flap.
func TestPlanDye_IsDeterministic(t *testing.T) {
	files := []manifestedit.FileContent{
		{
			Path:    "b/kustomization.yaml",
			Content: []byte("resources:\n  - d.yaml\nimages:\n  - name: b\n    newTag: v1\n"),
		},
		{
			Path:    "a/kustomization.yaml",
			Content: []byte("resources:\n  - d.yaml\nimages:\n  - name: a\n    newTag: v1\n"),
		},
	}

	first := planDye(files)
	for range 5 {
		require.Equal(t, first.replace, planDye(files).replace)
	}
}
