// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The applier claim kinds as kubectl names them.
const (
	fluxClaimResource   = "fluxclaim"
	argoCDClaimResource = "argocdclaim"
)

// createFluxClaim creates the FluxClaim name in namespace, holding the Kustomization
// flux-system/<kustomization> in the operator's own cluster (the install's "local" KubeProvider).
func createFluxClaim(namespace, name, kustomization string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: FluxClaim
metadata:
  name: %s
  namespace: %s
spec:
%s
  kustomizationRef:
    namespace: flux-system
    name: %s
`, name, namespace, localKubeProviderRef(), kustomization)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to create FluxClaim %s/%s", namespace, name)
}

// createArgoCDClaim creates the ArgoCDClaim name in namespace, holding the Application
// <appNamespace>/<application> in the operator's own cluster.
func createArgoCDClaim(namespace, name, appNamespace, application string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: ArgoCDClaim
metadata:
  name: %s
  namespace: %s
spec:
%s
  applicationRef:
    namespace: %s
    name: %s
`, name, namespace, localKubeProviderRef(), appNamespace, application)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to create ArgoCDClaim %s/%s", namespace, name)
}

// grantAppliersInAnyNamespace lets the applier claims that reach their applier through the install's
// KubeProvider hold one in any namespace, as the e2e appliers are: in flux-system and argocd, while
// each spec's applier claims live in its own namespace. Granting it is idempotent, and no spec
// relies on its absence on that provider, so parallel specs may all ask for it.
func grantAppliersInAnyNamespace() {
	GinkgoHelper()
	_, err := kubectlRunInNamespace(resolveE2ENamespace(), "patch", "kubeprovider", e2eKubeProviderName,
		"--type=merge", "-p", `{"spec":{"allowedApplierClaims":[{"from":"All","applierNamespaces":["*"]}]}}`)
	Expect(err).NotTo(HaveOccurred(), "failed to let the install KubeProvider's applier claims reach other namespaces")
}

// suiteKubeProviderGrants are the grants the suite needs on the install's KubeProvider: every
// namespace mirrors its own namespace and any cluster-scoped resource, and holds an applier in its
// own namespace. config/kubeprovider-local.yaml carries them, and the Helm install passes them as
// values (test/e2e/helm-values-suite-grants.yaml); the chart grants nothing by default, so the plain
// manifests need them patched in. On a config-dir install the patch restates what the manifest
// already carries.
const suiteKubeProviderGrants = `{"spec":{` +
	`"allowedGitTargets":[{"from":"All","sourceNamespaces":["{namespace}"],"clusterResources":["*"]}],` +
	`"allowedApplierClaims":[{"from":"All","applierNamespaces":["{namespace}"]}]}}`

// grantSuiteAccess gives the install's KubeProvider the grants the suite needs. A Helm install already
// carries them as values, and a patch there would take the fields from Helm, whose next server-side
// upgrade then refuses the conflict.
func grantSuiteAccess() {
	GinkgoHelper()
	if mode, err := resolveE2EInstallMode(); err == nil && mode == "helm" {
		return
	}
	_, err := kubectlRunInNamespace(resolveE2ENamespace(), "patch", "kubeprovider", e2eKubeProviderName,
		"--type=merge", "-p", suiteKubeProviderGrants)
	Expect(err).NotTo(HaveOccurred(), "failed to grant the suite's access on the install KubeProvider")
}

// nameApplierClaim points a GitTarget at the applier claim of kind (FluxClaim or ArgoCDClaim) and
// name, and sets onRefusal when it is not empty.
func nameApplierClaim(namespace, gitTarget, kind, claim, onRefusal string) {
	GinkgoHelper()
	refusal := ""
	if onRefusal != "" {
		refusal = fmt.Sprintf(`,"onRefusal":%q`, onRefusal)
	}
	_, err := kubectlRunInNamespace(namespace, "patch", "gittarget", gitTarget, "--type=merge", "-p",
		fmt.Sprintf(`{"spec":{"applierClaimRef":{"kind":%q,"name":%q}%s}}`, kind, claim, refusal))
	Expect(err).NotTo(HaveOccurred(), "failed to point GitTarget %s/%s at %s %s", namespace, gitTarget, kind, claim)
}

// requestClaimReconcile asks an applier claim's reconciler to read its applier now. It reads the
// applier on its own cadence and watches nothing in the applier's cluster, so a test that changes
// the applier asks for the read rather than wait for it.
func requestClaimReconcile(namespace, resource, name string) {
	GinkgoHelper()
	_, err := kubectlRunInNamespace(namespace, "annotate", resource, name, "--overwrite",
		"reconcile.configbutler.ai/requestedAt="+strconv.FormatInt(time.Now().UnixNano(), 10))
	Expect(err).NotTo(HaveOccurred(), "failed to request a %s reconcile", resource)
}

// claimOperation is an applier claim's status.operation: its reason, commit, phase and token, as the
// branch worker writes them on every transition.
type claimOperation struct {
	Reason, Commit, Phase, Token string
}

// readClaimOperation reads an applier claim's status.operation; the zero value when it has none.
func readClaimOperation(g Gomega, namespace, resource, name string) claimOperation {
	out, err := kubectlRunInNamespace(namespace, "get", resource, name, "-o",
		"jsonpath={.status.operation.reason}|{.status.operation.commit}|{.status.operation.phase}|"+
			"{.status.operation.token}")
	g.Expect(err).NotTo(HaveOccurred())
	const fields = 4
	parts := strings.SplitN(strings.TrimSpace(out), "|", fields)
	for len(parts) < fields {
		parts = append(parts, "")
	}
	return claimOperation{Reason: parts[0], Commit: parts[1], Phase: parts[2], Token: parts[3]}
}

// fluxSourceStamp is the reconcile.fluxcd.io/requestedAt of a GitRepository in flux-system: set
// only when something asked it to fetch.
func fluxSourceStamp(name string) string {
	GinkgoHelper()
	out, err := kubectlRunInNamespace("flux-system", "get", "gitrepository", name, "-o",
		`jsonpath={.metadata.annotations.reconcile\.fluxcd\.io/requestedAt}`)
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// kustomizationSuspended reads a flux-system Kustomization's spec.suspend.
func kustomizationSuspended(g Gomega, name string) bool {
	out, err := kubectlRunInNamespace("flux-system", "get", "kustomization", name, "-o", "jsonpath={.spec.suspend}")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out) == "true"
}

// kustomizationManagers lists the field managers of a flux-system Kustomization's managedFields.
func kustomizationManagers(g Gomega, name string) string {
	out, err := kubectlRunInNamespace("flux-system", "get", "kustomization", name, "-o",
		"jsonpath={.metadata.managedFields[*].manager}", "--show-managed-fields")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// fluxClaimEvents is every message of the Events with reason recorded on the FluxClaim name.
func fluxClaimEvents(g Gomega, namespace, name, reason string) string {
	out, err := kubectlRunInNamespace(namespace, "get", "events", "--field-selector",
		"involvedObject.kind=FluxClaim,involvedObject.name="+name+",reason="+reason,
		"-o", "jsonpath={.items[*].message}")
	g.Expect(err).NotTo(HaveOccurred())
	return out
}
