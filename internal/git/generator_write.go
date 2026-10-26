// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/manifestreport"
)

// writeGenerated writes a live ConfigMap that a configMapGenerator renders back into the
// generator's input files: a files: source gets the live value as its content, an envs: line is
// edited in place. manifestanalyzer.PlanGeneratedWrite decides where each key goes; the oracle
// then re-renders the folder and refuses the flush unless the generator produces the live object.
//
// A change with no destination (binaryData, a label, a key with no single input to land in) is
// refused rather than written somewhere else: a new ConfigMap document beside the generator would
// collide with it in the build.
func (wb *writeBatch) writeGenerated(
	ctx context.Context,
	event Event,
	generated []*manifestanalyzer.GeneratedObject,
) (upsertOutcome, error) {
	live := event.Object
	gen := generated[0]
	if len(generated) > 1 {
		return upsertNoChange, generatedRefusal(live, gen.GeneratedIn, fmt.Errorf(
			"%d configMapGenerator entries render an object of this name", len(generated)))
	}
	if wb.writer.isSensitiveIdentifier(event.Identifier) {
		// A generator input is plaintext; a resource the target encrypts must never land in one.
		return upsertNoChange, generatedRefusal(live, gen.GeneratedIn, errors.New(
			"the resource is sensitive, and a generator input is a plaintext file"))
	}

	desired := manifestreport.Project(live)
	// The same render-fidelity fence a document's edit passes: a ${...} token the input holds that
	// the live object carries expanded was substituted downstream (Flux postBuild), and writing the
	// expansion back into the input is the corruption no re-render can see, since it then matches.
	divergences := manifestanalyzer.RenderTokenDivergences(gen.Rendered.Object, desired.Object)
	if len(divergences) > 0 {
		id := manifestedit.Identity{Kind: desired.GetKind(), Name: desired.GetName()}
		return upsertNoChange, renderFidelityRefusal(
			gen.GeneratedIn, id, &renderFidelityRefusedError{Divergences: divergences})
	}
	writes, err := manifestanalyzer.PlanGeneratedWrite(gen, desired, wb.writeSubdir, wb.currentContent)
	if err != nil {
		return upsertNoChange, generatedRefusal(live, gen.GeneratedIn, err)
	}
	for _, w := range writes {
		wb.buffer(w.Path).current = w.Content
		log.FromContext(ctx).Info("Routed live change to configMapGenerator input",
			"kustomization", gen.GeneratedIn, "file", w.Path, "resource", event.Identifier.String())
	}

	// Declared whether or not this event wrote anything, as for a governed document: an input
	// may be shared with another entry an earlier event already wrote, and only the intent tells
	// the oracle that this object's move is convergence. Governed, because a generated object no
	// render root produces after the write is a contradiction, not a skip.
	wb.intend(manifestanalyzer.WriteIntent{
		SourcePath: gen.GeneratedIn,
		Kind:       desired.GetKind(),
		Name:       desired.GetName(),
		Desired:    desired,
		Governed:   true,
	})
	if len(writes) == 0 {
		return upsertNoChange, nil
	}
	wb.putToKustomize = true
	return upsertUpdated, nil
}

// deleteGenerated mirrors a live delete of an object a configMapGenerator renders: the entry
// leaves configMapGenerator:, and the input files only it reads are deleted, so the next render
// does not bring the object back. The oracle proves the object left the render and nothing else
// moved. A delete no generator renders is a no-op.
func (wb *writeBatch) deleteGenerated(ctx context.Context, event Event) error {
	obj := event.Object
	if obj == nil {
		id := event.Identifier
		if id.Group != "" || id.Resource != "configmaps" {
			return nil
		}
		obj = &unstructured.Unstructured{}
		obj.SetAPIVersion("v1")
		obj.SetKind("ConfigMap")
		obj.SetNamespace(id.Namespace)
		obj.SetName(id.Name)
	}
	generated := wb.store.GeneratedObjectsFor(obj)
	switch {
	case len(generated) == 0:
		return nil
	case len(generated) > 1:
		return generatedRefusal(obj, generated[0].GeneratedIn, fmt.Errorf(
			"%d configMapGenerator entries render an object of this name", len(generated)))
	}
	gen := generated[0]
	writes, err := manifestanalyzer.PlanGeneratedDelete(gen, wb.writeSubdir, wb.currentContent)
	if err != nil {
		return generatedRefusal(obj, gen.GeneratedIn, err)
	}
	for _, w := range writes {
		buf := wb.buffer(w.Path)
		buf.current = w.Content
		if w.Path == gen.GeneratedIn {
			wb.recordDocumentRemoval(buf)
		}
	}
	wb.intend(manifestanalyzer.WriteIntent{
		SourcePath: gen.GeneratedIn,
		Kind:       obj.GetKind(),
		Name:       obj.GetName(),
		Removed:    true,
	})
	wb.putToKustomize = true
	wb.tallyDocument(event.Identifier, documentDeletedLive)
	log.FromContext(ctx).Info("Removed configMapGenerator entry for a deleted ConfigMap",
		"kustomization", gen.GeneratedIn, "files", len(writes)-1, "resource", event.Identifier.String())
	return nil
}

// generatedRefusal reports a generated object the writer cannot write back, as the write-boundary
// refusal every unplaceable edit surfaces as, naming the object and the kustomization.
func generatedRefusal(obj *unstructured.Unstructured, kustomization string, err error) error {
	return &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{
			Kind: manifestanalyzer.IssueUnplaceableEdit,
			Path: kustomization,
			// Not solvable from either side: the change itself has nowhere to land.
			Solvable: false,
			Message: fmt.Sprintf("%s/%s generated in %s: %v",
				obj.GetKind(), obj.GetName(), kustomization, err),
		}},
	}
}

// currentContent is a file's bytes as the batch has left them so far, without hydrating a buffer
// for it.
func (wb *writeBatch) currentContent(rel string) ([]byte, bool) {
	if b, ok := wb.buffers[rel]; ok {
		return b.current, b.current != nil
	}
	content, ok := wb.contentByPath[rel]
	return content, ok
}
