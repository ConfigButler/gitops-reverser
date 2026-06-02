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
	crdGroupCRDLifecycle    = "crd-lifecycle.e2e.example.com"
	crdGroupRestartSnapshot = "restart-snapshot.e2e.example.com"
	crdGroupBiDirectional   = "bi-directional.e2e.example.com"
	crdGroupWildcardRule    = "wildcard-watchrule.e2e.example.com"
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
