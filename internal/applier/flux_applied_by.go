// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
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

// What else applies a GitRepository (docs/design/apply-trigger.md, "Shared sources"). A suspended
// Kustomization ignores a new artifact; an object applying the source that is not held applies each
// new artifact the moment it arrives, so a FluxClaim lists the ones no applier claim holds in its
// status.alsoAppliedBy.

// A HelmRelease applies a GitRepository through a HelmChart that source-controller builds from it:
// one helm-controller creates from the release's spec.chart, or one the release names in
// spec.chartRef. Both kinds are listed at the newest version the cluster serves, out of the
// versions Flux has served them at, so an older Flux is read too.
const (
	kindHelmRelease = "HelmRelease"
	kindHelmChart   = "HelmChart"
)

// helmReleaseGVR is Flux's HelmRelease at its current version.
func helmReleaseGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
}

// helmChartGVR is source-controller's HelmChart at its current version.
func helmChartGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmcharts"}
}

// olderHelmReleaseVersions and olderHelmChartVersions are the versions Flux served the kinds at
// before their current one, newest first.
func olderHelmReleaseVersions() []string { return []string{"v2beta2", "v2beta1"} }
func olderHelmChartVersions() []string   { return []string{"v1beta2"} }

// AppliedBy lists the objects in the cluster that apply the GitRepository source, sorted:
// Kustomizations whose spec.sourceRef names it, and HelmReleases whose chart is built from it,
// through spec.chart.spec.sourceRef or through a HelmChart spec.chartRef names. Any failure is an
// error, and then nothing is known: the caller must treat the source as shared.
//
// A kind the cluster does not serve has no objects. For HelmReleases that holds only when no
// HelmChart is built from the source either: a release at a version not listed here would still
// apply through one, so then what applies the source is unknown and that is an error.
func (f *Flux) AppliedBy(
	ctx context.Context,
	source types.NamespacedName,
) ([]fluxmeta.NamespacedObjectKindReference, error) {
	kustomizations, _, err := f.listServed(ctx, kustomizationGVR())
	if err != nil {
		return nil, err
	}
	var appliedBy []fluxmeta.NamespacedObjectKindReference
	for i := range kustomizations {
		if consumes(&kustomizations[i], []string{"spec", "sourceRef"}, source) {
			appliedBy = append(appliedBy, kindReference(kindKustomization, &kustomizations[i]))
		}
	}
	releases, err := f.helmReleasesOf(ctx, source)
	if err != nil {
		return nil, err
	}
	appliedBy = append(appliedBy, releases...)
	sort.Slice(appliedBy, func(i, j int) bool {
		a, b := appliedBy[i], appliedBy[j]
		return a.Kind+"/"+a.Namespace+"/"+a.Name < b.Kind+"/"+b.Namespace+"/"+b.Name
	})
	return appliedBy, nil
}

// helmReleasesOf is the HelmReleases whose chart is built from the GitRepository source.
func (f *Flux) helmReleasesOf(
	ctx context.Context,
	source types.NamespacedName,
) ([]fluxmeta.NamespacedObjectKindReference, error) {
	releases, served, err := f.listServed(ctx, helmReleaseGVR(), olderHelmReleaseVersions()...)
	if err != nil {
		return nil, err
	}
	if !served {
		charts, _, err := f.chartsBuiltFrom(ctx, source)
		if err != nil {
			return nil, err
		}
		if len(charts) > 0 {
			return nil, fmt.Errorf("%d %s(s) are built from %s %s, but the cluster serves %ss at no version known here",
				len(charts), kindHelmChart, kindGitRepository, source, kindHelmRelease)
		}
		return nil, nil
	}
	var appliedBy []fluxmeta.NamespacedObjectKindReference
	var viaChart []*unstructured.Unstructured
	for i := range releases {
		release := &releases[i]
		switch {
		case consumes(release, []string{"spec", "chart", "spec", "sourceRef"}, source):
			appliedBy = append(appliedBy, kindReference(kindHelmRelease, release))
		case namesHelmChart(release):
			viaChart = append(viaChart, release)
		}
	}
	if len(viaChart) == 0 {
		return appliedBy, nil
	}
	charts, served, err := f.chartsBuiltFrom(ctx, source)
	if err != nil {
		return nil, err
	}
	if !served {
		return nil, fmt.Errorf("%ss name a %s in spec.chartRef, but the cluster serves %ss at no version known here",
			kindHelmRelease, kindHelmChart, kindHelmChart)
	}
	for _, release := range viaChart {
		if charts[chartRefOf(release)] {
			appliedBy = append(appliedBy, kindReference(kindHelmRelease, release))
		}
	}
	return appliedBy, nil
}

// chartsBuiltFrom is the HelmCharts whose spec.sourceRef names the GitRepository source; a
// HelmChart's source is always in its own namespace. served is false when the cluster serves no
// HelmChart version, and then none is built at all.
func (f *Flux) chartsBuiltFrom(
	ctx context.Context,
	source types.NamespacedName,
) (map[types.NamespacedName]bool, bool, error) {
	charts, served, err := f.listServed(ctx, helmChartGVR(), olderHelmChartVersions()...)
	if err != nil || !served {
		return nil, served, err
	}
	built := map[types.NamespacedName]bool{}
	for i := range charts {
		if consumes(&charts[i], []string{"spec", "sourceRef"}, source) {
			built[types.NamespacedName{Namespace: charts[i].GetNamespace(), Name: charts[i].GetName()}] = true
		}
	}
	return built, true, nil
}

// listServed lists every object of gvr, or of the same resource at the first of the older versions
// the cluster serves; served is false when it serves none of them.
func (f *Flux) listServed(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	older ...string,
) ([]unstructured.Unstructured, bool, error) {
	for _, version := range append([]string{gvr.Version}, older...) {
		gvr.Version = version
		list, err := f.client.Resource(gvr).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("list %s: %w", gvr.GroupResource(), err)
		}
		return list.Items, true, nil
	}
	return nil, false, nil
}

// namesHelmChart reports whether a HelmRelease's spec.chartRef names a HelmChart.
func namesHelmChart(release *unstructured.Unstructured) bool {
	kind, _, _ := unstructured.NestedString(release.Object, "spec", "chartRef", "kind")
	return kind == kindHelmChart
}

// chartRefOf is the HelmChart a HelmRelease's spec.chartRef names, its namespace defaulted to the
// release's own, as Flux defaults it.
func chartRefOf(release *unstructured.Unstructured) types.NamespacedName {
	ref, _, _ := unstructured.NestedStringMap(release.Object, "spec", "chartRef")
	namespace := ref["namespace"]
	if namespace == "" {
		namespace = release.GetNamespace()
	}
	return types.NamespacedName{Namespace: namespace, Name: ref["name"]}
}

func kindReference(kind string, obj *unstructured.Unstructured) fluxmeta.NamespacedObjectKindReference {
	return fluxmeta.NamespacedObjectKindReference{Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName()}
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
