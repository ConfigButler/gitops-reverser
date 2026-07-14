// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"errors"
	"fmt"
	"path"
	"regexp"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	kustypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// This file renders a kustomize render root with kustomize itself, rather than
// re-implementing its transformers. It is the ground truth the projection is
// checked against: what we believe a folder renders to becomes what the library
// Flux renders with says it renders to.
//
// See docs/design/support-boundary/kustomize-support-boundary.md §7.

// The provenance kustomize emits when buildMetadata asks for it. These are the
// reason we need not walk the resources graph ourselves:
//
//	config.kubernetes.io/origin:        path: ../base/deployment.yaml
//	alpha.config.kubernetes.io/transformations:
//	  - configuredIn: ../base/kustomization.yaml
//	    configuredBy: {apiVersion: builtin, kind: ImageTagTransformer}
//
// The first says which source file produced the object. The second says which
// kustomization's transformers RAN over it, in build order — the override chain,
// handed to us by the renderer that applies it. (Ran over, not touched: kustomize
// records a transformer against every object in the build, whether or not it changed
// anything. See chainOf.)
const (
	originAnnotation          = "config.kubernetes.io/origin"
	transformationsAnnotation = "alpha.config.kubernetes.io/transformations"
)

// imageTagTransformer and replicaCountTransformer are the builtin transformers
// behind the two edit-through channels, as kustomize names them in the
// transformations annotation.
const (
	imageTagTransformer     = "ImageTagTransformer"
	replicaCountTransformer = "ReplicaCountTransformer"
)

// errRemoteBase refuses a build whose kustomization reaches outside the repository.
//
// The check must run BEFORE krusty, never inside it: kustomize resolves a remote
// base by shelling out to `git fetch`, and it does so under LoadRestrictionsRootOnly
// and under an in-memory filesystem alike (both measured). No build option turns it
// off, so refusing first is the only thing that keeps "the operator never fetches a
// remote base" true.
var errRemoteBase = errors.New("kustomization reaches a remote base; the operator never fetches one")

// errInvalidImageName refuses a build whose images: entry carries a name kustomize
// cannot compile.
//
// An images: entry's name: is a REGULAR EXPRESSION, not a literal, and kustomize
// compiles it while DISCARDING the compile error (api/internal/image/image.go):
//
//	pattern, _ := regexp.Compile("^" + name + "(:[a-zA-Z0-9_.{}-]*)?(@sha256:...)?$")
//
// It then dereferences the nil *Regexp. So `- name: "ngin["` does not fail the build —
// it PANICS inside it, on content that came straight from a user's repository. Like the
// remote-base check, this one must run before krusty, and for the same reason: it is not
// a modelling question, it is what keeps a hostile kustomization.yaml from taking the
// process somewhere it cannot come back from.
var errInvalidImageName = errors.New("images: entry name is not a valid regular expression")

// errBuildPanicked is the net under krusty. errInvalidImageName covers the one panic we
// found; this covers the ones we have not. A build runs library code we do not own over
// bytes we do not control, so a panic there has to become a refused folder — never a
// crashed CLI, and never a GitTarget that panics, requeues and panics again for as long
// as the repository stays as it is (controller-runtime recovers reconciler panics by
// default, which turns a crash into a hot loop, not into safety).
var errBuildPanicked = errors.New("kustomize build panicked")

// imageNamePattern is the pattern kustomize compiles for an images: entry name
// (api/internal/image/image.go). We validate the WHOLE pattern, not the name alone, so
// that what we accept is exactly what kustomize can compile.
func imageNamePattern(name string) string {
	return "^" + name + "(:[a-zA-Z0-9_.{}-]*)?(@sha256:[a-zA-Z0-9_.{}-]*)?$"
}

// renderedObject is one object kustomize produced, with the provenance saying
// where it came from and what shaped it.
type renderedObject struct {
	// Object is the rendered result: what the GitOps controller will apply.
	Object *unstructured.Unstructured
	// OriginPath is the source file that produced it, relative to the scan root.
	// Empty for a generated resource (which the acceptance gate refuses anyway).
	OriginPath string
	// TransformedBy lists the kustomizations whose transformers touched it, in
	// build order (innermost base first) — the override chain, from kustomize.
	TransformedBy []transformation
}

// transformation is one entry of kustomize's transformations annotation: which
// kustomization configured which builtin transformer.
type transformation struct {
	// ConfiguredIn is the kustomization file, relative to the scan root.
	ConfiguredIn string
	// Kind is the builtin transformer, e.g. "ImageTagTransformer".
	Kind string
}

// renderMountPoint is where the scanned tree is mounted in the in-memory
// filesystem. The whole scan is mounted, not just the render root, so a
// kustomization can read a base beside or below it. This is a READ scope: the
// write jail is enforced in the writer (L1) and is not this function's job.
const renderMountPoint = "/scan"

// renderRoot builds one render root with kustomize and returns every object it
// produces, carrying provenance. rootDir is slash-relative to the scan root, and
// files are the scan's YAML files, which become an in-memory filesystem — so the
// build never touches the real disk, never executes a plugin, and never reaches the
// network.
func renderRoot(files []manifestedit.FileContent, rootDir string) ([]renderedObject, error) {
	return renderRootWith(files, rootDir, nil)
}

// renderRootWith is renderRoot over a COUNTERFACTUAL tree: replace layers in-memory
// content for any scanned path, and the build sees that instead of what the scan holds.
//
// It is the whole new API this workstream needs, and every question we ask kustomize is
// this call with a different overlay:
//
//   - dye an entry and see where the nonce lands       -> attribution;
//   - apply a proposed write and see what it renders   -> verification.
//
// The point is that the counterfactual goes through the SAME sandbox, the same refusals
// and the same kustomize invocation as the real render. A question answered by a
// different renderer than the one that produces the answer we ship is not an answer.
//
// files is never mutated: callers hold the scan, and a probe must not be able to corrupt it.
// See docs/design/support-boundary/render-attribution.md §7.
func renderRootWith(
	files []manifestedit.FileContent,
	rootDir string,
	replace map[string][]byte,
) ([]renderedObject, error) {
	input := replacedFiles(files, replace)
	if err := refuseBeforeBuild(parseKustomizations(input), rootDir); err != nil {
		return nil, err
	}
	fSys, err := renderFilesystem(input, rootDir)
	if err != nil {
		return nil, err
	}
	resMap, err := build(fSys, path.Join(renderMountPoint, rootDir))
	if err != nil {
		return nil, fmt.Errorf("kustomize build: %w", err)
	}
	return collectRendered(resMap, rootDir)
}

// replacedFiles returns a copy of files with replace layered over it, keyed by slash path.
//
// A replacement for a path the scan does not hold is ignored rather than appended: a
// counterfactual may only perturb files that are actually in the tree kustomize is about
// to build, and inventing one would be a way to render something the repository does not
// contain.
func replacedFiles(files []manifestedit.FileContent, replace map[string][]byte) []manifestedit.FileContent {
	if len(replace) == 0 {
		return files
	}
	out := append([]manifestedit.FileContent(nil), files...)
	for i := range out {
		if content, ok := replace[filepathToSlash(out[i].Path)]; ok {
			out[i].Content = content
		}
	}
	return out
}

// build runs krusty, converting a panic into an error (errBuildPanicked).
//
// LoadRestrictionsNone is what Flux itself builds with, and it is safe here for the same
// reason it is safe there: THE FILESYSTEM IS THE JAIL. The in-memory filesystem contains
// only the scanned tree, so "unrestricted" loading cannot reach the real disk, and a
// remote base is refused before we get here.
//
// RootOnly would be the wrong kind of strict. It forbids loading a FILE from outside the
// render root — `resources: [../shared.yaml]` — which Flux renders happily. We would then
// fail to build a root that deploys in production, see no chain for it, and quietly stop
// enforcing write-fan-in on the file it shares. Refusing to look is not a safety property.
func build(fSys filesys.FileSystem, target string) (_ resmap.ResMap, err error) {
	// On a panic the result is the zero ResMap (nil) and err is what the deferred
	// function leaves here, so only the error needs naming.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", errBuildPanicked, r)
		}
	}()
	k := krusty.MakeKustomizer(&krusty.Options{
		LoadRestrictions: kustypes.LoadRestrictionsNone,
		PluginConfig:     kustypes.DisabledPluginConfig(), // no exec, no Go plugins
	})
	return k.Run(fSys, target)
}

// refuseBeforeBuild refuses the build when any kustomization THIS ROOT REACHES is one we
// must not hand to krusty: it declares a remote base (kustomize would fetch it), or an
// images: entry name kustomize would nil-deref on (see errInvalidImageName).
//
// Scoping it to the reachable graph is deliberate, and it is both safer and more
// accurate than a scan-wide check: kustomize only loads what it actually reaches,
// so a remote base in an unrelated sibling folder cannot make this build reach the
// network — and refusing on its account would refuse a folder that is perfectly
// renderable.
func refuseBeforeBuild(kusts map[string]*kustomizationDoc, rootDir string) error {
	visited := map[string]struct{}{}
	var walk func(dir string) error
	walk = func(dir string) error {
		if _, seen := visited[dir]; seen {
			return nil
		}
		visited[dir] = struct{}{}
		cur := kusts[dir]
		if cur == nil {
			return nil
		}
		if err := unbuildable(cur); err != nil {
			return err
		}
		for _, entry := range cur.resources {
			target := cleanJoin(dir, entry)
			if target == "" {
				continue
			}
			if _, isKust := kusts[target]; isKust {
				if err := walk(target); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(rootDir)
}

// unbuildable reports why one kustomization must not be handed to krusty, or nil when
// it may be. Both reasons are properties of the BUILD, not of what we can model: a
// kustomization we refuse here is one kustomize would fetch the network for, or crash on.
func unbuildable(k *kustomizationDoc) error {
	if hasRemoteResource(k.resources) {
		return fmt.Errorf("%s: %w", k.path, errRemoteBase)
	}
	for _, img := range k.images {
		if _, err := regexp.Compile(imageNamePattern(img.Name)); err != nil {
			return fmt.Errorf("%s: images[%d].name %q: %w", k.path, img.Index, img.Name, errInvalidImageName)
		}
	}
	return nil
}

// renderFilesystem materialises the scan's files in memory and asks the render root
// for provenance.
func renderFilesystem(files []manifestedit.FileContent, rootDir string) (filesys.FileSystem, error) {
	fSys := filesys.MakeFsInMemory()

	for _, f := range files {
		rel := filepathToSlash(f.Path)
		content := f.Content

		// Only the root needs to ask for provenance: the annotations describe the
		// whole build, bases included.
		//
		// The root is matched the way the rest of the analyzer matches one —
		// isKustomizationFile, which accepts kustomization.yml as well. Comparing
		// against the literal "kustomization.yaml" instead left a .yml root building
		// happily with NO buildMetadata: no origin, no transformations, so every
		// object came back with an empty OriginPath and an empty override chain, and
		// a governed image was written into the source manifest for the overlay to
		// shadow straight back.
		if slashDir(rel) == rootDir && isKustomizationFile(rel) {
			var k kustypes.Kustomization
			if err := k.Unmarshal(content); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			k.FixKustomization()
			var err error
			if content, err = withBuildMetadata(k); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
		}
		if err := fSys.WriteFile(path.Join(renderMountPoint, rel), content); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
	}
	return fSys, nil
}

// withBuildMetadata re-serialises a kustomization with the provenance build
// metadata added. It rewrites only our in-memory render copy; the user's file is
// never touched, so losing their comments here costs nothing.
func withBuildMetadata(k kustypes.Kustomization) ([]byte, error) {
	k.BuildMetadata = []string{kustypes.OriginAnnotations, kustypes.TransformerAnnotations}
	out, err := yaml.Marshal(&k)
	if err != nil {
		return nil, fmt.Errorf("re-serialising kustomization for render: %w", err)
	}
	return out, nil
}

// collectRendered turns kustomize's ResMap into rendered objects, lifting the
// provenance off each one and then stripping it: the annotations are our
// scaffolding, not part of what the folder renders to, and an object carrying them
// would compare unequal to the live object it describes.
func collectRendered(resMap resmap.ResMap, rootDir string) ([]renderedObject, error) {
	out := make([]renderedObject, 0, resMap.Size())
	for _, res := range resMap.Resources() {
		m, err := res.Map()
		if err != nil {
			return nil, fmt.Errorf("reading rendered resource: %w", err)
		}
		obj := &unstructured.Unstructured{Object: m}
		ro := renderedObject{
			Object:        obj,
			OriginPath:    originOf(obj, rootDir),
			TransformedBy: transformationsOf(obj, rootDir),
		}
		unstructured.RemoveNestedField(obj.Object, "metadata", "annotations", originAnnotation)
		unstructured.RemoveNestedField(obj.Object, "metadata", "annotations", transformationsAnnotation)
		if len(obj.GetAnnotations()) == 0 {
			unstructured.RemoveNestedField(obj.Object, "metadata", "annotations")
		}
		out = append(out, ro)
	}
	return out, nil
}

// originOf reads the source file an object was rendered from, normalised from
// render-root-relative ("../base/deployment.yaml") to scan-root-relative.
func originOf(obj *unstructured.Unstructured, rootDir string) string {
	raw := obj.GetAnnotations()[originAnnotation]
	if raw == "" {
		return ""
	}
	var origin struct {
		Path string `json:"path"`
	}
	if err := yaml.Unmarshal([]byte(raw), &origin); err != nil || origin.Path == "" {
		return ""
	}
	return path.Clean(path.Join(rootDir, origin.Path))
}

// transformationsOf reads the ordered transformer chain kustomize applied, so the
// writer knows which kustomizations govern this object and in what order.
func transformationsOf(obj *unstructured.Unstructured, rootDir string) []transformation {
	raw := obj.GetAnnotations()[transformationsAnnotation]
	if raw == "" {
		return nil
	}
	var entries []struct {
		ConfiguredIn string `json:"configuredIn"`
		ConfiguredBy struct {
			Kind string `json:"kind"`
		} `json:"configuredBy"`
	}
	if err := yaml.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	out := make([]transformation, 0, len(entries))
	for _, e := range entries {
		if e.ConfiguredIn == "" {
			continue
		}
		out = append(out, transformation{
			ConfiguredIn: path.Clean(path.Join(rootDir, e.ConfiguredIn)),
			Kind:         e.ConfiguredBy.Kind,
		})
	}
	return out
}
