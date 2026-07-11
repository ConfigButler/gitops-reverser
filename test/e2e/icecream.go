// SPDX-License-Identifier: Apache-2.0

package e2e

// Per-file IceCreamOrder CRD groups. Every e2e file that installs the
// IceCreamOrder CRD owns its own API group so the files no longer share
// cluster-scoped state and can run on parallel Ginkgo processes (Phase 2.5 of
// docs/finished/e2e-speedup-plan.md). The kind, plural and singular stay the same
// across all of them; only the group differs. Because the plural
// ("icecreamorders") is shared, kubectl resource references MUST be qualified as
// "icecreamorders.<group>" (see iceCreamCRDName) to avoid ambiguity when more
// than one of these CRDs is installed concurrently.
const (
	crdGroupCRDLifecycle      = "crd-lifecycle.e2e.example.com"
	crdGroupRestartReconcile  = "restart-reconcile.e2e.example.com"
	crdGroupBiDirectional     = "bi-directional.e2e.example.com"
	crdGroupArgoBiDirectional = "argo-bi-directional.e2e.example.com"
	crdGroupWildcardRule      = "wildcard-watchrule.e2e.example.com"
)

// iceCreamCRDGroups returns every group declared above, plus the legacy
// pre-isolation group. prepareE2EClusterOnce deletes all of them before the run
// so a warm/reused cluster starts clean; adding a group above is enough.
func iceCreamCRDGroups() []string {
	return []string{
		crdGroupCRDLifecycle,
		crdGroupRestartReconcile,
		crdGroupBiDirectional,
		crdGroupArgoBiDirectional,
		crdGroupWildcardRule,
		"shop.example.com", // legacy pre-isolation group
	}
}

// iceCreamCRDName returns the fully-qualified CRD name for the given group. This
// doubles as the unambiguous kubectl resource selector (e.g.
// "icecreamorders.crd-lifecycle.e2e.example.com").
func iceCreamCRDName(group string) string {
	return "icecreamorders." + group
}

// iceCreamCRDMirrorFile returns the filename gitops-reverser writes for the CRD
// under _cluster/apiextensions.k8s.io/customresourcedefinitions/ (CRDs are
// cluster-scoped, so the scope segment is the literal "_cluster").
func iceCreamCRDMirrorFile(group string) string {
	return iceCreamCRDName(group) + ".yaml"
}

// iceCreamInstancePath returns the canonical mirror path for one IceCreamOrder
// instance under the new namespace-first, version-less layout:
// "<namespace>/<group>/icecreamorders/<name>.yaml".
func iceCreamInstancePath(group, namespace, name string) string {
	return namespace + "/" + group + "/icecreamorders/" + name + ".yaml"
}

// applyIceCreamCRD renders and applies the IceCreamOrder CRD for the given group.
func applyIceCreamCRD(group string) error {
	return applyFromTemplate(
		"test/e2e/templates/icecreamorder-crd.tmpl",
		struct{ Group string }{Group: group},
		"",
	)
}
