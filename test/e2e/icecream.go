// SPDX-License-Identifier: Apache-2.0

package e2e

// Per-file IceCreamOrder CRD groups. Every e2e file that installs the
// IceCreamOrder CRD owns its own API group so the three files no longer share
// cluster-scoped state and can run on parallel Ginkgo processes (Phase 2.5 of
// docs/design/e2e-speedup-plan.md). The kind, plural and singular stay the same
// across all three; only the group differs. Because the plural ("icecreamorders")
// is shared, kubectl resource references MUST be qualified as
// "icecreamorders.<group>" (see iceCreamCRDName) to avoid ambiguity when more
// than one of these CRDs is installed concurrently.
const (
	crdGroupCRDLifecycle     = "crd-lifecycle.e2e.example.com"
	crdGroupRestartReconcile = "restart-reconcile.e2e.example.com"
	crdGroupBiDirectional    = "bi-directional.e2e.example.com"
	crdGroupWildcardRule     = "wildcard-watchrule.e2e.example.com"
)

// iceCreamCRDName returns the fully-qualified CRD name for the given group. This
// doubles as the unambiguous kubectl resource selector (e.g.
// "icecreamorders.crd-lifecycle.e2e.example.com").
func iceCreamCRDName(group string) string {
	return "icecreamorders." + group
}

// iceCreamCRDMirrorFile returns the filename gitops-reverser writes for the CRD
// under apiextensions.k8s.io/v1/customresourcedefinitions/.
func iceCreamCRDMirrorFile(group string) string {
	return iceCreamCRDName(group) + ".yaml"
}

// iceCreamInstanceDir returns the mirror path prefix for IceCreamOrder instances
// of the given group: "<group>/v1/icecreamorders".
func iceCreamInstanceDir(group string) string {
	return group + "/v1/icecreamorders"
}

// applyIceCreamCRD renders and applies the IceCreamOrder CRD for the given group.
func applyIceCreamCRD(group string) error {
	return applyFromTemplate(
		"test/e2e/templates/icecreamorder-crd.tmpl",
		struct{ Group string }{Group: group},
		"",
	)
}
