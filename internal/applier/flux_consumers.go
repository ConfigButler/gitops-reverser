// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"errors"
	"fmt"
	"sort"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// A GitRepository's consumers, and prefetching it (docs/design/apply-trigger.md, "Shared sources and prefetch").
// A suspended Kustomization ignores a new artifact, so a GitRepository whose every consumer is held
// suspended by a FluxTrigger can fetch on every push without applying anything. One consumer that is
// not held applies each new artifact the moment it arrives, so then the source is never prefetched.

// helmReleaseGVR is Flux's HelmRelease, whose chart can come from a GitRepository.
func helmReleaseGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
}

const kindHelmRelease = "HelmRelease"

// Consumers lists the objects in the cluster that apply the GitRepository source: Kustomizations
// whose spec.sourceRef names it, and HelmReleases whose spec.chart.spec.sourceRef does, sorted. A kind
// the cluster does not serve has none. Any other failure is an error, and then nothing is known: the
// caller must treat the source as shared.
func (f *Flux) Consumers(
	ctx context.Context,
	source types.NamespacedName,
) ([]fluxmeta.NamespacedObjectKindReference, error) {
	var consumers []fluxmeta.NamespacedObjectKindReference
	for _, kind := range []struct {
		name string
		gvr  schema.GroupVersionResource
		path []string
	}{
		{kindKustomization, kustomizationGVR(), []string{"spec", "sourceRef"}},
		{kindHelmRelease, helmReleaseGVR(), []string{"spec", "chart", "spec", "sourceRef"}},
	} {
		list, err := f.client.Resource(kind.gvr).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list %ss: %w", kind.name, err)
		}
		for i := range list.Items {
			if consumes(&list.Items[i], kind.path, source) {
				consumers = append(consumers, fluxmeta.NamespacedObjectKindReference{
					Kind: kind.name, Namespace: list.Items[i].GetNamespace(), Name: list.Items[i].GetName(),
				})
			}
		}
	}
	sort.Slice(consumers, func(i, j int) bool {
		a, b := consumers[i], consumers[j]
		return a.Kind+"/"+a.Namespace+"/"+a.Name < b.Kind+"/"+b.Namespace+"/"+b.Name
	})
	return consumers, nil
}

// consumes reports whether obj's source reference at path names the GitRepository source, its
// namespace defaulted to obj's own, as Flux defaults it.
func consumes(obj *unstructured.Unstructured, path []string, source types.NamespacedName) bool {
	ref, found, _ := unstructured.NestedStringMap(obj.Object, path...)
	if !found || ref["kind"] != kindGitRepository || ref["name"] != source.Name {
		return false
	}
	namespace := ref["namespace"]
	if namespace == "" {
		namespace = obj.GetNamespace()
	}
	return namespace == source.Namespace
}

// errNotPrefetched is a prefetch that was not made, because the artifact already holds the commit.
var errNotPrefetched = errors.New("the artifact already holds the commit")

// Prefetch stamps the GitRepository of the Kustomization ref so it fetches now, unless its artifact
// already holds commit (errNotPrefetched). It waits for nothing: the operation that applies the
// commit reads the artifact again before it resumes.
func (f *Flux) Prefetch(ctx context.Context, ref types.NamespacedName, commit string) error {
	ks, err := f.client.Resource(kustomizationGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get %s %s: %w", kindKustomization, ref, err)
	}
	sourceRef, err := gitRepositoryRef(ks)
	if err != nil {
		return err
	}
	source, err := f.client.Resource(gitRepositoryGVR()).Namespace(sourceRef.Namespace).
		Get(ctx, sourceRef.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get %s %s: %w", kindGitRepository, sourceRef, err)
	}
	if commit != "" && artifactHolds(source, commit) {
		return errNotPrefetched
	}
	_, err = f.stampSource(ctx, sourceRef)
	return err
}
