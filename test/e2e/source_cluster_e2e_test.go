// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This file is the RED-FIRST scaffold for the config-plane split
// (docs/design/config-plane-split.md): a GitTarget may name the cluster it mirrors
// FROM via spec.kubeConfig (Flux's meta.KubeConfigReference).
//
// The feature does not exist yet. These specs are written first, on purpose:
//   - They COMPILE today, because CRs are applied as untyped YAML — a spec.kubeConfig
//     block is just a string until the CRD gains the field.
//   - They are DORMANT in the default suite (BeforeAll calls skipUnlessSourceClusterEnabled).
//   - Run them with E2E_ENABLE_SOURCE_CLUSTER=true and they FAIL (red) against the
//     current, feature-less operator; they go GREEN as the feature lands.
//
// The two-cluster GVK->GVR spec additionally needs a small SECOND k3d cluster whose
// kubeconfig is reachable from the operator pod, provided via
// E2E_SOURCE_CLUSTER_KUBECONFIG; without it that one spec skips (the harness infra is
// not built yet). See the "E2E and integration test plan" in the design doc.

const (
	sourceClusterEnabledEnv    = "E2E_ENABLE_SOURCE_CLUSTER"
	secondClusterKubeConfigEnv = "E2E_SOURCE_CLUSTER_KUBECONFIG"
	// unreachableAPIServer is an RFC 5737 TEST-NET-1 address: syntactically a valid
	// server, guaranteed not to route, so a kubeconfig pointing at it parses cleanly
	// (Validated=True) yet can never be dialed (SourceClusterReachable=False).
	unreachableAPIServer = "https://192.0.2.1:6443"
	// inClusterAPIServer is reachable from inside the management cluster's pod network,
	// so a kubeconfig whose only change is this server drives the whole remote path
	// (resolver -> clusterContext -> watch) against the cluster the operator runs in.
	inClusterAPIServer = "https://kubernetes.default.svc:443"
)

func sourceClusterEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(sourceClusterEnabledEnv)))
	return v == "1" || v == "true" || v == "yes"
}

func skipUnlessSourceClusterEnabled() {
	GinkgoHelper()
	if !sourceClusterEnabled() {
		Skip(fmt.Sprintf(
			"config-plane split is disabled; set %s=true to run these specs "+
				"(they are red until GitTarget.spec.kubeConfig ships)", sourceClusterEnabledEnv))
	}
}

// rawKubeConfigWithServer returns the current cluster's real, self-contained kubeconfig
// (embedded CA + client credential) with only the API server address swapped. Swapping to
// an unroutable address yields "valid but unreachable"; swapping to the in-cluster address
// yields a self-referencing "remote" that actually works from the operator pod.
func rawKubeConfigWithServer(server string) string {
	GinkgoHelper()
	raw, err := kubectlRun("config", "view", "--raw", "--minify", "-o", "yaml")
	Expect(err).NotTo(HaveOccurred(), "failed to read current kubeconfig")
	out := make([]string, 0, strings.Count(raw, "\n")+1)
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "server:") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			out = append(out, indent+"server: "+server)
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// insecureKubeConfig is structurally valid but disables TLS verification — the operator
// must reject it (KubeConfigInsecureTLSNotAllowed), diverging from Flux's silent strip.
func insecureKubeConfig() string {
	return `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: ` + unreachableAPIServer + `
    insecure-skip-tls-verify: true
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user:
    token: dummy-token
`
}

// execKubeConfig is structurally valid but carries an exec auth provider — the operator
// must reject it (KubeConfigExecNotAllowed): an exec stanza runs a binary in the Pod.
func execKubeConfig() string {
	return `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: ` + unreachableAPIServer + `
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /bin/echo
      args: ["token"]
`
}

// writeKubeConfigSecret applies a Secret holding a kubeconfig under the given key.
func writeKubeConfigSecret(ns, name, key, kubeconfig string) {
	GinkgoHelper()
	f, err := os.CreateTemp("", "e2e-kubeconfig-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.WriteString(kubeconfig)
	Expect(err).NotTo(HaveOccurred())
	Expect(f.Close()).To(Succeed())

	manifest, err := kubectlRunInNamespace(ns, "create", "secret", "generic", name,
		"--from-file="+key+"="+f.Name(), "--dry-run=client", "-o", "yaml")
	Expect(err).NotTo(HaveOccurred(), "failed to render kubeconfig Secret manifest")
	_, err = kubectlRunWithStdin(ns, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply kubeconfig Secret")
}

// applyGitTargetWithKubeConfig applies a GitTarget whose spec.kubeConfig.secretRef names a
// kubeconfig Secret. It returns the kubectl error so a spec can distinguish an admission
// rejection (the red state before the CRD field exists) from a later status assertion.
func applyGitTargetWithKubeConfig(ns, name, provider, path, secretName, key string) (string, error) {
	keyLine := ""
	if key != "" {
		// key: must be a sibling of name: (6-space indent, under secretRef:), not nested
		// under it — an 8-space indent produces "mapping values are not allowed here".
		keyLine = "\n      key: " + key
	}
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: %s
  namespace: %s
spec:
  providerRef:
    kind: GitProvider
    name: %s
  branch: main
  path: %s
  kubeConfig:
    secretRef:
      name: %s%s
`, name, ns, provider, path, secretName, keyLine)
	return kubectlRunWithStdin(ns, manifest, "apply", "-f", "-")
}

// findFileByBasename walks a checkout and returns the first path whose basename matches,
// so a mirror assertion need not hard-code the exact placement path.
func findFileByBasename(root, basename string) string {
	var hit string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && filepath.Base(p) == basename {
			hit = p
		}
		return nil
	})
	return hit
}

var _ = Describe("Manager source cluster / config-plane split", Label("source-cluster"), Ordered, func() {
	const providerName = "sc-provider"

	var (
		testNs string
		repo   *RepoArtifacts
	)

	BeforeAll(func() {
		skipUnlessSourceClusterEnabled()

		testNs = testNamespaceFor("source-cluster")
		_, _ = kubectlRun("create", "namespace", testNs)

		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-source-cluster-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply repo secrets")

		createReadyGitProvider(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
	})

	AfterAll(func() { cleanupNamespace(testNs) })

	SetDefaultEventuallyTimeout(60 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// Scenario 1 — input validation is legible, and never dials.
	inputCases := []struct {
		name   string
		reason string
		setup  func(ns string) (secretName, key string)
	}{
		{
			name:   "a missing Secret",
			reason: "KubeConfigSecretNotFound",
			setup:  func(_ string) (string, string) { return "sc-absent", "" },
		},
		{
			name:   "a missing key",
			reason: "KubeConfigKeyNotFound",
			setup: func(ns string) (string, string) {
				kubeconfig := rawKubeConfigWithServer(unreachableAPIServer)
				writeKubeConfigSecret(ns, "sc-wrongkey", "somewhere-else", kubeconfig)
				return "sc-wrongkey", "value"
			},
		},
		{
			name:   "an unparseable kubeconfig",
			reason: "KubeConfigInvalid",
			setup: func(ns string) (string, string) {
				writeKubeConfigSecret(ns, "sc-garbage", "value", "this is not a kubeconfig")
				return "sc-garbage", ""
			},
		},
		{
			name:   "an exec auth provider",
			reason: "KubeConfigExecNotAllowed",
			setup: func(ns string) (string, string) {
				writeKubeConfigSecret(ns, "sc-exec", "value", execKubeConfig())
				return "sc-exec", ""
			},
		},
		{
			name:   "insecure TLS",
			reason: "KubeConfigInsecureTLSNotAllowed",
			setup: func(ns string) (string, string) {
				writeKubeConfigSecret(ns, "sc-insecure", "value", insecureKubeConfig())
				return "sc-insecure", ""
			},
		},
	}
	for _, tc := range inputCases {
		It("fails Validated (no dial) for "+tc.name+" with reason "+tc.reason, func() {
			secretName, key := tc.setup(testNs)
			target := "sc-input-" + strings.ToLower(tc.reason)
			path := "clusters/input/" + tc.reason
			_, _ = applyGitTargetWithKubeConfig(testNs, target, providerName, path, secretName, key)
			verifyResourceCondition("gittarget", target, testNs, "Validated", "False", tc.reason, "")
		})
	}

	// Scenario 2 — a valid kubeconfig that cannot be dialed: Validated=True, reachability=False.
	It("separates Validated (inputs) from SourceClusterReachable (runtime)", func() {
		writeKubeConfigSecret(testNs, "sc-unreachable", "value", rawKubeConfigWithServer(unreachableAPIServer))
		target := "sc-unreachable-target"
		_, err := applyGitTargetWithKubeConfig(
			testNs, target, providerName, "clusters/unreachable", "sc-unreachable", "")
		Expect(err).NotTo(HaveOccurred())

		verifyResourceCondition("gittarget", target, testNs, "Validated", "True", "OK", "")
		verifyResourceCondition("gittarget", target, testNs,
			"SourceClusterReachable", "False", "SourceClusterUnreachable", "", "150s")
	})

	// Scenario 3 — omitted kubeConfig is unchanged local behavior.
	It("treats an omitted kubeConfig as the local cluster", func() {
		target := "sc-local-target"
		manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata: {name: %s, namespace: %s}
spec:
  providerRef: {kind: GitProvider, name: %s}
  branch: main
  path: clusters/local
`, target, testNs, providerName)
		_, err := kubectlRunWithStdin(testNs, manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())

		verifyResourceCondition("gittarget", target, testNs,
			"SourceClusterReachable", "True", "LocalCluster", "")
	})

	// Scenario 4 — a self-referencing "remote": the whole remote path on one cluster.
	It("mirrors through a self-referencing remote kubeconfig", func() {
		writeKubeConfigSecret(testNs, "sc-self", "value", rawKubeConfigWithServer(inClusterAPIServer))
		target := "sc-self-target"
		_, err := applyGitTargetWithKubeConfig(testNs, target, providerName, "clusters/self", "sc-self", "")
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("gittarget", target, testNs, "SourceClusterReachable", "True", "", "", "150s")

		ruleManifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata: {name: sc-self-rule, namespace: %s}
spec:
  targetRef: {kind: GitTarget, name: %s}
  rules:
  - resources: ["configmaps"]
`, testNs, target)
		_, err = kubectlRunWithStdin(testNs, ruleManifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		waitForStreamsRunning(target, testNs)

		_, err = kubectlRunInNamespace(testNs, "create", "configmap", "sc-self-cm", "--from-literal=hello=world")
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			g.Expect(findFileByBasename(repo.CheckoutDir, "sc-self-cm.yaml")).
				NotTo(BeEmpty(), "expected the ConfigMap mirrored from the self-referencing remote")
		}).WithTimeout(120 * time.Second).Should(Succeed())
	})

	// Scenario 8 — the centerpiece: GVK->GVR resolution is source-cluster scoped, proven
	// by making two clusters legitimately DISAGREE on one GVK. Needs a real second cluster
	// (E2E_SOURCE_CLUSTER_KUBECONFIG). Dormant until that harness infra lands.
	//
	// Local:  example.io/v1 Widget served as `widgets`  (Namespaced).
	// Remote: example.io/v1 Widget served as `widgetz`  (Cluster-scoped).
	// A remote GitTarget watching Widget must mirror the remote object at the REMOTE's
	// identity: a path under .../example.io/widgetz/... at cluster scope. A union / first-
	// wins lookup would resolve against the LOCAL registry and file it under
	// {namespace}/example.io/widgets/... — wrong plural AND wrong scope.
	It("resolves GVK->GVR against the source cluster, not a union", func() {
		kubeconfigPath := strings.TrimSpace(os.Getenv(secondClusterKubeConfigEnv))
		if kubeconfigPath == "" {
			Skip(fmt.Sprintf(
				"needs a second k3d cluster reachable from the operator pod; set %s to its kubeconfig "+
					"(second-cluster harness not implemented yet — see the design doc test plan)",
				secondClusterKubeConfigEnv))
		}

		// --- Intended body once the second-cluster harness exists (kept explicit so the
		// implementer wires provisioning, not test logic): ---
		//  1. Install CRD widgets.example.io  (kind Widget, Namespaced) on the LOCAL cluster.
		//  2. Install CRD widgetz.example.io  (kind Widget, Cluster-scoped) on the REMOTE.
		//  3. writeKubeConfigSecret(testNs, "sc-widget-remote", "value", <remote kubeconfig>).
		//  4. applyGitTargetWithKubeConfig(..., "clusters/widget", "sc-widget-remote", "")
		//     + a ClusterWatchRule selecting example.io/v1 Widget; waitForStreamsRunning.
		//  5. Create a Widget on the REMOTE cluster (kubectl --kubeconfig=<remote>).
		//  6. Assert the mirrored file path contains "example.io/widgetz" at cluster scope,
		//     and assert it does NOT appear under a "widgets" / namespaced path (the union bug).
		Fail("second-cluster GVK->GVR scenario is scaffolded but not yet runnable; " +
			"provisioning helper is the remaining infra (design doc: E2E test plan)")
	})

	// Scenarios 5 (credential rotation), 6 (credential-reference authorization / admission
	// SubjectAccessReview) and 7 (GitProviderReady projection) are described in the design
	// doc's E2E test plan and land alongside the corresponding controller/webhook code.
})
