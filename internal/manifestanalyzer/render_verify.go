// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// This file is the oracle: before a commit that routes anything through a kustomization,
// we render the repository as it would stand AFTER the commit and refuse it unless the
// answer is exactly the one we intended.
//
// ATTRIBUTION MAY BE HEURISTIC. VERIFICATION MAY NOT. Deciding which file an edit belongs
// in is an inference, and this workstream is deliberately replacing one inference with a
// better one. But the check that says "and the result is right" must not be an inference at
// all, because a check that shares the blind spot of the thing it checks turns a wrong guess
// into a CONFIDENT wrong write.
//
// That is precisely what the deleted simulateImageRender did: it replayed OUR chain over the
// images OUR projection had planned, so a case our model got wrong, it got wrong twice — and
// agreed with itself. This cannot do that. It runs the library Flux renders with, over the
// exact bytes we are about to commit.
//
// It runs ONCE PER FLUSH, not once per resource, and that is a correctness requirement
// rather than an optimisation. An images: entry is shared context: converging one Deployment
// through it necessarily moves every other object the entry matches. Asked resource by
// resource, "did anything else move?" is yes for the first of two Deployments that share an
// image and are being bumped together — so a per-resource oracle would refuse a write that
// converges perfectly well. Only the whole batch knows that the sibling is moving because it,
// too, is being written to exactly where it now lives.
//
// See docs/design/support-boundary/render-attribution.md §5 and
// docs/design/support-boundary/render-root-scoping.md §3.

// WriteIntent is one document a flush writes, and what the render must show for it
// afterwards. Everything the batch does NOT declare an intent for must come out of the
// render byte-for-byte unchanged — that is the blast-radius half of the oracle, and it is
// what makes it safe for the projection to guess.
type WriteIntent struct {
	// SourcePath is the file holding the document, slash-relative to the scan root.
	SourcePath string
	// Kind and Name identify the document within that file.
	Kind, Name string
	// Desired is the live object the document must render to. It is the whole point of
	// the check, and it is nil only when Removed or Unchecked says there is nothing to
	// check against.
	Desired *unstructured.Unstructured
	// Removed marks a document the flush deletes: it must disappear from the render.
	Removed bool
	// Unchecked marks a write whose rendered form we cannot predict, so the object is
	// permitted to move without being compared. There are exactly two: a SENSITIVE
	// document (the file is SOPS-encrypted, so kustomize renders the ciphertext, which
	// no plaintext live object can equal), and a bounded FIELD PATCH (the event carries
	// a few assignments, never a whole object to compare against).
	//
	// Unchecked weakens the oracle for those documents, and it is stated rather than
	// hidden: we can still prove that such a write disturbs nothing ELSE, which is the
	// half that protects other people's environments.
	Unchecked bool
	// Governed marks a document whose write was routed through a kustomization override
	// chain. These are the writes the oracle exists for, so one that turns out not to be
	// rendered by any root at all is a contradiction, and refused rather than skipped.
	Governed bool
}

func (w WriteIntent) key() chainKey {
	return chainKey{originPath: filepathToSlash(w.SourcePath), kind: w.Kind, name: w.Name}
}

// RenderRefusedError is the oracle's verdict: the bytes the flush was about to commit do
// not render to the live cluster state, or they move something the flush never intended to
// touch. It aborts the flush — nothing is written — and it names the file and the object,
// because the correct outcome here is a REPORTED refusal, never a resource that is quietly
// not mirrored (render-attribution.md §7).
type RenderRefusedError struct {
	// Reasons are the individual findings, sorted, so the message is stable across runs.
	Reasons []string
}

func (e *RenderRefusedError) Error() string {
	return "kustomize render refused the write: " + strings.Join(e.Reasons, "; ")
}

// VerifyBatchRenders re-renders every root of the subtree twice — as the flush found it,
// and as the flush would leave it — and proves both halves of the oracle:
//
//  1. every document the flush writes renders to exactly the live object, and
//  2. every object it does NOT write is byte-for-byte unchanged.
//
// (2) is not a nicety. A kustomization is shared context: an images: entry edited to
// converge one Deployment governs every other object it matches, and a base is rendered by
// every overlay above it. A proposal that fixes its own target and moves a second object has
// written a live value into a file another render root also reads — the one edit write-fan-in
// exists to forbid.
//
// before and after are complete file trees. The cost is two builds per render root, once per
// flush, and it is only paid when the flush routed something through a kustomization.
func VerifyBatchRenders(before, after []manifestedit.FileContent, intents []WriteIntent) error {
	byKey := make(map[chainKey]WriteIntent, len(intents))
	for _, in := range intents {
		byKey[in.key()] = in
	}
	seen := map[chainKey]struct{}{}

	var reasons []string
	for _, root := range renderTargets(parseKustomizations(after)) {
		was, err := renderRoot(before, root)
		if err != nil {
			// The tree did not build BEFORE we touched it. The acceptance gate refuses
			// such a folder, so we should never be writing into one — but an unverifiable
			// root is not a verified root, so say so rather than skip it.
			reasons = append(reasons, fmt.Sprintf("render root %s did not build before the write: %v", root, err))
			continue
		}
		now, err := renderRoot(after, root)
		if err != nil {
			reasons = append(
				reasons,
				fmt.Sprintf("render root %s no longer builds with the write applied: %v", root, err),
			)
			continue
		}
		reasons = append(reasons, compareRoot(root, byKey, seen, renderedByKey(was), renderedByKey(now))...)
	}

	for _, in := range intents {
		if _, hit := seen[in.key()]; in.Governed && !hit {
			reasons = append(reasons, fmt.Sprintf(
				"%s/%s in %s was routed through a kustomization override, but no render root renders it",
				in.Kind, in.Name, in.SourcePath))
		}
	}

	if len(reasons) == 0 {
		return nil
	}
	sort.Strings(reasons)
	return &RenderRefusedError{Reasons: reasons}
}

// compareRoot checks one render root's before/after pair against the flush's intents.
func compareRoot(
	root string,
	intents map[chainKey]WriteIntent,
	seen map[chainKey]struct{},
	was, now map[chainKey]renderedObject,
) []string {
	var reasons []string
	for key := range unionKeys(was, now) {
		before, existed := was[key]
		after, exists := now[key]
		intent, intended := intents[key]
		if intended {
			seen[key] = struct{}{}
		}

		switch {
		case !intended:
			// The blast radius. This object is nobody's target, so the flush has no
			// business changing it — in this root or any other.
			if !existed || !exists {
				reasons = append(reasons, fmt.Sprintf(
					"the write adds or removes %s/%s in render root %s, which it never set out to write",
					key.kind, key.name, root))
				continue
			}
			if !sameObject(before.Object, after.Object) {
				reasons = append(reasons, fmt.Sprintf(
					"the write also changes what render root %s renders for %s/%s (from %s), "+
						"which it never set out to write",
					root, key.kind, key.name, key.originPath))
			}
		case intent.Removed:
			if exists {
				reasons = append(reasons, fmt.Sprintf(
					"%s/%s was deleted, but render root %s still renders it", key.kind, key.name, root))
			}
		case intent.Unchecked:
			// A sensitive document or a bounded field patch: it is allowed to move, and
			// there is nothing to compare it against. The other cases above still hold it
			// to not disturbing anything else.
		case !exists:
			reasons = append(reasons, fmt.Sprintf(
				"%s/%s was written, but render root %s no longer renders it", key.kind, key.name, root))
		case !sameObject(after.Object, intent.Desired):
			reasons = append(reasons, fmt.Sprintf(
				"in render root %s, %s/%s (from %s) does not render to the live object after the write",
				root, key.kind, key.name, key.originPath))
		}
	}
	return reasons
}

func unionKeys(a, b map[chainKey]renderedObject) map[chainKey]struct{} {
	out := make(map[chainKey]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

// sameObject compares two rendered/live objects by canonical JSON.
//
// Canonical JSON rather than reflect.DeepEqual on purpose: kustomize hands numbers back as
// Go int where the API machinery uses int64 — measured, and it is why
// unstructured.NestedInt64 reports found=false on a rendered spec.replicas — and a
// comparison that called those two different would refuse every replica write there is.
// json.Marshal also sorts map keys, so this is key-order independent.
func sameObject(a, b *unstructured.Unstructured) bool {
	if a == nil || b == nil {
		return a == b
	}
	left, err := json.Marshal(a.Object)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b.Object)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

// renderedByKey indexes a render by the document each object came from. The key carries the
// ORIGIN FILE, not just kind/name, so two same-named objects rendered from different source
// files stay distinct.
func renderedByKey(objects []renderedObject) map[chainKey]renderedObject {
	out := make(map[chainKey]renderedObject, len(objects))
	for _, o := range objects {
		out[chainKey{originPath: o.OriginPath, kind: o.Object.GetKind(), name: o.Object.GetName()}] = o
	}
	return out
}
