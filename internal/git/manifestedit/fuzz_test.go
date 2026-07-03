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

package manifestedit

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kyaml "sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
)

// FuzzManifestEdit is the project's dynamic-analysis (fuzzing) gate for the
// manifest-editing surface — the hand-rolled byte manipulation that turns
// untrusted, semi-structured YAML into edits (document splitting, block-scalar
// detection, decoding, indexing, the decide/apply pipeline, and deletion). It
// varies the input bytes and asserts the robustness invariant that must hold for
// every possible file: no entry point panics on arbitrary or malformed input.
//
// Convergence (a patched document settling to a byte-stable no-op) is a separate
// product property proven on realistic manifests by TestConvergence_Corpus. It is
// deliberately not asserted here: a faithful "desired" object comes from the typed
// Kubernetes API, whereas a fuzzer can only synthesize one by parsing arbitrary
// YAML, which yields type-ambiguous objects (e.g. numeric ConfigMap values) the
// real system never produces — so convergence over fuzzer-built desired states
// tests the harness, not the code.
//
// Seed inputs cover the shapes most likely to surprise the splitter and decoder;
// the seed corpus is replayed by a plain `go test` (no -fuzz), so these keep
// working as ordinary regression cases with no dedicated CI job. A fuzz-found
// crasher is kept under testdata/fuzz/FuzzManifestEdit/ as a regression case.
func FuzzManifestEdit(f *testing.F) {
	seeds := []string{
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: default\ndata:\n  k: v\n",
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: d\nspec:\n  replicas: 1\n",
		"---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n" +
			"---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: b\n",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  note: |\n    ---\n    not a separator\n",
		"apiVersion: v1\nkind: X\nmetadata: {name: a}\nx: &anchor 1\ny: *anchor\n", // disallowed constructs
		"not: [valid, yaml", // malformed
		"# comment only\n",  // empty document
		"",                  // empty file
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(_ *testing.T, data []byte) {
		// Read-only surfaces must never panic on arbitrary bytes.
		_ = DocumentCount(data)
		_, _ = IndexFile("fuzz.yaml", data)

		for idx := range splitDocuments(string(data)) {
			_, _ = NewDocument(data, idx)
			// Deletion is content-agnostic and must never panic, whatever the doc holds.
			_, _ = DeleteDocument(data, idx)
			// The decide/apply/patch pipeline must not panic either. Exercise it with a
			// realistic desired projection (parsed like production, then sanitized) so
			// the edit actually runs instead of skipping at the door.
			fuzzExerciseEdit(data, idx)
		}
	})
}

// fuzzExerciseEdit drives the full content-edit pipeline for one document with a
// production-shaped desired object, purely to shake out panics. It asserts
// nothing: correctness of the edit (convergence, byte-fidelity) is covered by the
// package's regular tests on realistic inputs.
func fuzzExerciseEdit(content []byte, idx int) {
	docs := splitDocuments(string(content))
	if idx < 0 || idx >= len(docs) {
		return
	}

	// Build the desired projection the way production does: parse to a JSON-clean
	// object via the Kubernetes YAML->JSON path, then run it through the same
	// sanitizer the live writer uses. kyaml.Unmarshal guarantees string keys and
	// standard types, so this construction cannot itself panic on adversarial YAML.
	var m map[string]interface{}
	if err := kyaml.Unmarshal([]byte(docs[idx].body), &m); err != nil || m == nil {
		return
	}
	desired := sanitize.Sanitize(&unstructured.Unstructured{Object: m})
	if desired.GetAPIVersion() == "" || desired.GetKind() == "" || desired.GetName() == "" {
		return
	}

	opts := EditOptions{Render: sanitize.MarshalToOrderedYAML}
	_, _ = PatchDocument(content, idx, desired, opts)
}
