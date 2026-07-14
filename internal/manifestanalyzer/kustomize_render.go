// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"errors"
	"fmt"
	"path"

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
// kustomization's transformers touched it, in build order — the override chain,
// handed to us by the renderer that applies it.
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
	if err := refuseRemoteBases(parseKustomizations(files), rootDir); err != nil {
		return nil, err
	}
	fSys, err := renderFilesystem(files, rootDir)
	if err != nil {
		return nil, err
	}
	// LoadRestrictionsNone is what Flux itself builds with, and it is safe here for
	// the same reason it is safe there: THE FILESYSTEM IS THE JAIL. The in-memory
	// filesystem contains only the scanned tree, so "unrestricted" loading cannot
	// reach the real disk, and a remote base is refused before we get here.
	//
	// RootOnly would be the wrong kind of strict. It forbids loading a FILE from
	// outside the render root — `resources: [../shared.yaml]` — which Flux renders
	// happily. We would then fail to build a root that deploys in production, see no
	// chain for it, and quietly stop enforcing write-fan-in on the file it shares.
	// Refusing to look is not a safety property.
	k := krusty.MakeKustomizer(&krusty.Options{
		LoadRestrictions: kustypes.LoadRestrictionsNone,
		PluginConfig:     kustypes.DisabledPluginConfig(), // no exec, no Go plugins
	})
	resMap, err := k.Run(fSys, path.Join(renderMountPoint, rootDir))
	if err != nil {
		return nil, fmt.Errorf("kustomize build: %w", err)
	}
	return collectRendered(resMap, rootDir)
}

// refuseRemoteBases refuses the build when any kustomization THIS ROOT REACHES
// declares a remote base.
//
// Scoping it to the reachable graph is deliberate, and it is both safer and more
// accurate than a scan-wide check: kustomize only fetches what it actually loads,
// so a remote base in an unrelated sibling folder cannot make this build reach the
// network — and refusing on its account would refuse a folder that is perfectly
// renderable.
func refuseRemoteBases(kusts map[string]*kustomizationDoc, rootDir string) error {
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
		if hasRemoteResource(cur.resources) {
			return fmt.Errorf("%s: %w", cur.path, errRemoteBase)
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

// renderFilesystem materialises the scan's files in memory and asks the render root
// for provenance.
func renderFilesystem(files []manifestedit.FileContent, rootDir string) (filesys.FileSystem, error) {
	fSys := filesys.MakeFsInMemory()
	rootKust := path.Join(rootDir, "kustomization.yaml")

	for _, f := range files {
		rel := filepathToSlash(f.Path)
		content := f.Content

		// Only the root needs to ask for provenance: the annotations describe the
		// whole build, bases included.
		if rel == rootKust {
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
