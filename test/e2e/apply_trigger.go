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

// The apply trigger kinds as kubectl names them.
const (
	fluxTriggerResource   = "fluxtrigger"
	argoCDTriggerResource = "argocdtrigger"
)

// createFluxTrigger creates the FluxTrigger name in namespace, holding the Kustomization
// flux-system/<kustomization> in the operator's own cluster (the "default" ClusterProvider). An
// empty quietPeriod takes the default.
func createFluxTrigger(namespace, name, kustomization, quietPeriod string) {
	GinkgoHelper()
	quiet := ""
	if quietPeriod != "" {
		quiet = fmt.Sprintf("\n  quietPeriod: %q", quietPeriod)
	}
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: FluxTrigger
metadata:
  name: %s
  namespace: %s
spec:
  clusterProviderRef:
    name: default
  kustomizationRef:
    namespace: flux-system
    name: %s%s
`, name, namespace, kustomization, quiet)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to create FluxTrigger %s/%s", namespace, name)
}

// createArgoCDTrigger creates the ArgoCDTrigger name in namespace, holding the Application
// <appNamespace>/<application> in the operator's own cluster.
func createArgoCDTrigger(namespace, name, appNamespace, application string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: ArgoCDTrigger
metadata:
  name: %s
  namespace: %s
spec:
  clusterProviderRef:
    name: default
  applicationRef:
    namespace: %s
    name: %s
`, name, namespace, appNamespace, application)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to create ArgoCDTrigger %s/%s", namespace, name)
}

// nameApplyTrigger points a GitTarget at the trigger of kind (FluxTrigger or ArgoCDTrigger) and
// name, and sets onRefusal when it is not empty.
func nameApplyTrigger(namespace, gitTarget, kind, trigger, onRefusal string) {
	GinkgoHelper()
	refusal := ""
	if onRefusal != "" {
		refusal = fmt.Sprintf(`,"onRefusal":%q`, onRefusal)
	}
	_, err := kubectlRunInNamespace(namespace, "patch", "gittarget", gitTarget, "--type=merge", "-p",
		fmt.Sprintf(`{"spec":{"applyTriggerRef":{"kind":%q,"name":%q}%s}}`, kind, trigger, refusal))
	Expect(err).NotTo(HaveOccurred(), "failed to point GitTarget %s/%s at %s %s", namespace, gitTarget, kind, trigger)
}

// requestTriggerReconcile asks a trigger's reconciler to read its applier now. It reads the
// applier on its own cadence and watches nothing in the applier's cluster, so a test that changes
// the applier asks for the read rather than wait for it.
func requestTriggerReconcile(namespace, resource, name string) {
	GinkgoHelper()
	_, err := kubectlRunInNamespace(namespace, "annotate", resource, name, "--overwrite",
		"reconcile.configbutler.ai/requestedAt="+strconv.FormatInt(time.Now().UnixNano(), 10))
	Expect(err).NotTo(HaveOccurred(), "failed to request a %s reconcile", resource)
}

// triggerOperation is a trigger's status.operation: its reason, commit, phase and token, as the
// branch worker writes them on every transition.
type triggerOperation struct {
	Reason, Commit, Phase, Token string
}

// readTriggerOperation reads a trigger's status.operation; the zero value when it has none.
func readTriggerOperation(g Gomega, namespace, resource, name string) triggerOperation {
	out, err := kubectlRunInNamespace(namespace, "get", resource, name, "-o",
		"jsonpath={.status.operation.reason}|{.status.operation.commit}|{.status.operation.phase}|"+
			"{.status.operation.token}")
	g.Expect(err).NotTo(HaveOccurred())
	const fields = 4
	parts := strings.SplitN(strings.TrimSpace(out), "|", fields)
	for len(parts) < fields {
		parts = append(parts, "")
	}
	return triggerOperation{Reason: parts[0], Commit: parts[1], Phase: parts[2], Token: parts[3]}
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

// eventMessages is every message of the Events with reason recorded on the object kind/name.
func eventMessages(g Gomega, namespace, kind, name, reason string) string {
	out, err := kubectlRunInNamespace(namespace, "get", "events", "--field-selector",
		"involvedObject.kind="+kind+",involvedObject.name="+name+",reason="+reason,
		"-o", "jsonpath={.items[*].message}")
	g.Expect(err).NotTo(HaveOccurred())
	return out
}
