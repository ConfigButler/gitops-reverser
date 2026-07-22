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

// This file is the source-cluster corner for multi-cluster author attribution
// (docs/finished/multi-cluster-author-attribution.md): a GitTarget names the cluster it mirrors
// FROM by referencing a cluster-scoped ClusterProvider (spec.clusterProviderRef). The
// ClusterProvider is the home for that cluster's kubeconfig credential (spec.kubeConfig, a Flux
// meta.KubeConfigReference resolved from the operator namespace), namespace-access authorization,
// and connectivity status.
//
// Two kinds of spec live here:
//   - Input-validation specs (Scenario 1) assert the ClusterProvider reconciler's Validated
//     verdict on bad kubeconfigs, without a remote cluster and without a dial.
//   - Reachability specs (Scenarios 2-3) assert the GitTarget's SourceClusterReachable projection
//     for valid-but-unroutable and default (local) providers.
//   - Remote-mirror specs (Scenarios 4, 8) mirror real REMOTE clusters — kcp WORKSPACES, cheap
//     logical clusters installed by Flux (test/e2e/setup/kcp, see kcp_workspace_test.go).
//     Scenario 8 is the centerpiece: three workspaces holding the SAME namespace + resource with
//     different content, proving state is keyed by source cluster, not (namespace, GVR).
//
// The whole suite is gated by skipUnlessSourceClusterEnabled() (env E2E_ENABLE_SOURCE_CLUSTER),
// and the kcp specs additionally Skip when kcp is not installed — so a default `task test-e2e`
// (which installs no kcp) never turns them red. Run the corner with `task test-e2e-source-cluster`.

const (
	sourceClusterEnabledEnv = "E2E_ENABLE_SOURCE_CLUSTER"
	// unreachableAPIServer is an RFC 5737 TEST-NET-1 address: syntactically a valid
	// server, guaranteed not to route, so a kubeconfig pointing at it parses cleanly
	// (Validated=True) yet can never be dialed (SourceClusterReachable=False).
	unreachableAPIServer = "https://192.0.2.1:6443"
	// sourceClusterOperatorNS is the namespace a ClusterProvider's kubeConfig Secret is pinned to
	// (the operator's own namespace). A cluster-scoped provider has no namespace of its own.
	sourceClusterOperatorNS = defaultE2ENamespace
)

func sourceClusterEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(sourceClusterEnabledEnv)))
	return v == "1" || v == "true" || v == "yes"
}

func skipUnlessSourceClusterEnabled() {
	GinkgoHelper()
	if !sourceClusterEnabled() {
		Skip(fmt.Sprintf(
			"source-cluster corner is disabled; run `task test-e2e-source-cluster` "+
				"(sets %s=true and installs kcp)", sourceClusterEnabledEnv))
	}
}

// rawKubeConfigWithServer returns the current cluster's real, self-contained kubeconfig
// (embedded CA + client credential) with only the API server address swapped. Swapping to an
// unroutable address yields a "valid but unreachable" kubeconfig — Validated=True yet
// SourceClusterReachable=False — which is what the reachability specs assert. (Real remote
// mirroring is exercised against kcp workspaces, not a server swap; see kcp_workspace_test.go.)
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

// fileReferenceKubeConfig is structurally valid but names its token by FILE PATH — client-go
// would read that path from the operator Pod's own filesystem, so the operator must reject it
// (KubeConfigFileReferenceNotAllowed) rather than let a remote kubeconfig exfiltrate in-Pod files.
func fileReferenceKubeConfig() string {
	return `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: ` + unreachableAPIServer + `
    certificate-authority-data: dGVzdA==
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user:
    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
`
}

// writeKubeConfigSecret applies a Secret holding a kubeconfig under the given key, in the OPERATOR
// namespace — a ClusterProvider's secretRef is resolved from there, never from the source cluster.
func writeKubeConfigSecret(name, key, kubeconfig string) {
	GinkgoHelper()
	f, err := os.CreateTemp("", "e2e-kubeconfig-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.WriteString(kubeconfig)
	Expect(err).NotTo(HaveOccurred())
	Expect(f.Close()).To(Succeed())

	manifest, err := kubectlRunInNamespace(sourceClusterOperatorNS, "create", "secret", "generic", name,
		"--from-file="+key+"="+f.Name(), "--dry-run=client", "-o", "yaml")
	Expect(err).NotTo(HaveOccurred(), "failed to render kubeconfig Secret manifest")
	_, err = kubectlRunWithStdin(sourceClusterOperatorNS, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply kubeconfig Secret")
}

// applyClusterProvider applies a cluster-scoped ClusterProvider whose kubeConfig.secretRef names a
// kubeconfig Secret in the operator namespace, allowing the given namespace to reference it. It
// returns the kubectl error so a spec can assert the apply succeeded (the CR is well-formed; the
// kubeconfig, not the CR, is what a validation case makes bad).
func applyClusterProvider(name, secretName, key, allowedNS string) (string, error) {
	keyLine := ""
	if key != "" {
		keyLine = "\n      key: " + key
	}
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
metadata:
  name: %s
spec:
  kubeConfig:
    secretRef:
      name: %s%s
  allowedNamespaces:
    names: [%s]
`, name, secretName, keyLine, allowedNS)
	return kubectlRunWithStdin("", manifest, "apply", "-f", "-")
}

// deleteClusterProvider removes a cluster-scoped ClusterProvider (not covered by namespace cleanup).
func deleteClusterProvider(name string) {
	_, _ = kubectlRun("delete", "clusterprovider", name, "--ignore-not-found", "--wait=false")
}

// applyGitTargetWithClusterProvider applies a GitTarget whose spec.clusterProviderRef names a
// ClusterProvider (the source cluster it mirrors from).
//
//nolint:unparam // gitProvider is kept an explicit argument for readability; the corner uses one.
func applyGitTargetWithClusterProvider(ns, name, gitProvider, path, clusterProvider string) (string, error) {
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
  clusterProviderRef:
    name: %s
`, name, ns, gitProvider, path, clusterProvider)
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

var _ = Describe("Manager source cluster / ClusterProvider attribution", Label("source-cluster"), Ordered, func() {
	const providerName = "sc-provider"

	var (
		testNs string
		repo   *RepoArtifacts
		// kcp is the workspace harness for the remote-mirror specs. It is nil when kcp is not
		// installed (a default `task test-e2e` run) — those specs then Skip rather than fail.
		kcp *kcpTunnel
	)

	BeforeAll(func() {
		skipUnlessSourceClusterEnabled()

		testNs = testNamespaceFor("source-cluster")
		_, _ = kubectlRun("create", "namespace", testNs)

		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-source-cluster-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply repo secrets")

		createReadyGitProvider(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)

		if kcpAvailable() {
			kcp = startKcpTunnel()
		}
	})

	AfterAll(func() {
		if kcp != nil {
			kcp.stop()
		}
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(60 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// Scenario 1 — the ClusterProvider reconciler's input validation is legible, and never dials.
	inputCases := []struct {
		name   string
		reason string
		setup  func(cpName string) (secretName, key string)
	}{
		{
			name:   "a missing Secret",
			reason: "KubeConfigSecretNotFound",
			setup:  func(_ string) (string, string) { return "sc-absent", "" },
		},
		{
			name:   "a missing key",
			reason: "KubeConfigKeyNotFound",
			setup: func(cp string) (string, string) {
				writeKubeConfigSecret(cp+"-kc", "somewhere-else", rawKubeConfigWithServer(unreachableAPIServer))
				return cp + "-kc", "value"
			},
		},
		{
			name:   "an unparseable kubeconfig",
			reason: "KubeConfigInvalid",
			setup: func(cp string) (string, string) {
				writeKubeConfigSecret(cp+"-kc", "value", "this is not a kubeconfig")
				return cp + "-kc", ""
			},
		},
		{
			name:   "an exec auth provider",
			reason: "KubeConfigExecNotAllowed",
			setup: func(cp string) (string, string) {
				writeKubeConfigSecret(cp+"-kc", "value", execKubeConfig())
				return cp + "-kc", ""
			},
		},
		{
			name:   "insecure TLS",
			reason: "KubeConfigInsecureTLSNotAllowed",
			setup: func(cp string) (string, string) {
				writeKubeConfigSecret(cp+"-kc", "value", insecureKubeConfig())
				return cp + "-kc", ""
			},
		},
		{
			name:   "a file-path credential",
			reason: "KubeConfigFileReferenceNotAllowed",
			setup: func(cp string) (string, string) {
				writeKubeConfigSecret(cp+"-kc", "value", fileReferenceKubeConfig())
				return cp + "-kc", ""
			},
		},
	}
	for _, tc := range inputCases {
		It("fails ClusterProvider Validated (no dial) for "+tc.name+" with reason "+tc.reason, func() {
			cpName := "sc-input-" + strings.ToLower(tc.reason)
			secretName, key := tc.setup(cpName)
			DeferCleanup(func() { deleteClusterProvider(cpName) })
			// The ClusterProvider CR is well-formed (the kubeconfig is bad, not the CR), so the
			// apply itself must succeed — the controller then reports the typed reason on Validated.
			_, err := applyClusterProvider(cpName, secretName, key, testNs)
			Expect(err).NotTo(HaveOccurred())
			verifyResourceCondition("clusterprovider", cpName, "", "Validated", "False", tc.reason, "")
		})
	}

	// Scenario 2 — a valid kubeconfig that cannot be dialed: the ClusterProvider is Validated=True,
	// and the GitTarget that references it projects SourceClusterReachable=False.
	It("separates Validated (inputs) from SourceClusterReachable (runtime)", func() {
		const cpName = "sc-unreachable"
		writeKubeConfigSecret(cpName+"-kc", "value", rawKubeConfigWithServer(unreachableAPIServer))
		DeferCleanup(func() { deleteClusterProvider(cpName) })
		_, err := applyClusterProvider(cpName, cpName+"-kc", "", testNs)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("clusterprovider", cpName, "", "Validated", "True", "Validated", "")

		target := "sc-unreachable-target"
		_, err = applyGitTargetWithClusterProvider(testNs, target, providerName, "clusters/unreachable", cpName)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("gittarget", target, testNs, "Validated", "True", "Succeeded", "")
		verifyResourceCondition("gittarget", target, testNs,
			"SourceClusterReachable", "False", "SourceClusterUnreachable", "", "150s")
	})

	// Scenario 3 — the default (in-cluster) provider: an omitted clusterProviderRef defaults to
	// {name: default} and mirrors the operator's own cluster.
	It("treats an omitted clusterProviderRef as the default (local) cluster", func() {
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

	// Scenario 3b — the hard gate: a GitTarget that references a ClusterProvider which does NOT
	// exist is held NotReady (Validated=False, ClusterProviderNotFound) and never mirrors. This is
	// the regression for "clusterProvider.createDefault: false" — with no default provider a
	// GitTarget referencing it (or any missing provider) does not fall back to an implicit local
	// identity, and the operator never creates the object to rescue it.
	It("holds a GitTarget NotReady when its ClusterProvider does not exist (no bypass)", func() {
		target := "sc-missing-cp-target"
		_, err := applyGitTargetWithClusterProvider(
			testNs, target, providerName, "clusters/missing", "sc-does-not-exist")
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("gittarget", target, testNs,
			"Validated", "False", "ClusterProviderNotFound", "")
	})

	// Scenario 4 — a REAL remote cluster: mirror a ConfigMap out of one kcp workspace. Proves the
	// whole remote path end to end (ClusterProvider resolver -> per-cluster clientContext ->
	// per-cluster discovery -> per-cluster watch -> target-scoped writer) against an actually-remote
	// API, not the self-referencing in-cluster server the scaffold used to fake it with.
	It("mirrors a ConfigMap from a kcp workspace", func() {
		if kcp == nil {
			Skip("kcp is not installed; run this corner via `task test-e2e-source-cluster`")
		}
		const ws = "sc-mirror"
		hash := kcp.createWorkspace(ws)
		DeferCleanup(func() { kcp.cleanupWorkspaceTarget(testNs, ws) })
		DeferCleanup(func() { deleteClusterProvider(ws) })

		// A WatchRule watches its OWN namespace name on the source cluster, so the ConfigMap must
		// live under that namespace (testNs) IN the workspace — a separate cluster, so creating a
		// namespace of that name there is independent of the local one.
		_, err := kcp.wsKubectl(hash, "create", "namespace", testNs)
		Expect(err).NotTo(HaveOccurred(), "create watched namespace in the workspace")
		_, err = kcp.wsKubectl(hash, "-n", testNs, "create", "configmap", "sc-remote-cm",
			"--from-literal=hello=from-kcp")
		Expect(err).NotTo(HaveOccurred(), "create ConfigMap in the workspace")

		writeKubeConfigSecret(ws+"-kubeconfig", "value", kcp.operatorKubeConfig(hash))
		_, err = applyClusterProvider(ws, ws+"-kubeconfig", "", testNs)
		Expect(err).NotTo(HaveOccurred())
		target := ws + "-target"
		_, err = applyGitTargetWithClusterProvider(testNs, target, providerName, "clusters/kcp", ws)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("gittarget", target, testNs, "SourceClusterReachable", "True", "", "", "180s")

		rule := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata: {name: %s-rule, namespace: %s}
spec:
  targetRef: {kind: GitTarget, name: %s}
  rules:
  - resources: ["configmaps"]
`, ws, testNs, target)
		_, err = kubectlRunWithStdin(testNs, rule, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		waitForStreamsRunning(target, testNs)

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			hit := findFileByBasename(filepath.Join(repo.CheckoutDir, "clusters/kcp"), "sc-remote-cm.yaml")
			g.Expect(hit).NotTo(BeEmpty(), "expected the ConfigMap mirrored from the kcp workspace")
			content, readErr := os.ReadFile(hit)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).To(ContainSubstring("from-kcp"))
		}).WithTimeout(180 * time.Second).Should(Succeed())
	})

	// Scenario 8 — the centerpiece: source-cluster identity is load-bearing. Three workspaces each
	// hold the SAME namespace + the SAME resource name (demo/ConfigMap "shared") with DIFFERENT
	// content, mirrored by three GitTargets (each naming its own ClusterProvider) into three
	// folders. If the operator keyed state by (namespace, GVR) alone — a union / first-wins lookup —
	// the three identical identities would collapse into one; that they land as three distinct
	// files, each carrying its own workspace's value, is the proof that everything is keyed by
	// SOURCE CLUSTER (the ClusterProvider name).
	It("mirrors identical resources from three workspaces as distinct GitOps state", func() {
		if kcp == nil {
			Skip("kcp is not installed; run this corner via `task test-e2e-source-cluster`")
		}
		type wsCase struct{ ws, folder, value string }
		cases := []wsCase{
			{"sc-ws-a", "clusters/a", "alpha"},
			{"sc-ws-b", "clusters/b", "beta"},
			{"sc-ws-c", "clusters/c", "gamma"},
		}
		// Stand up all three mirrors, then assert on the OUTCOME (the mirrored files). We do not
		// gate on each target's StreamsRunning in the loop: that condition can lag its stream, and
		// the load-bearing claim here is what lands in Git, not the intermediate status. The final
		// Eventually is generous because three remote initial-snapshots run concurrently.
		for i := range cases {
			c := cases[i]
			hash := kcp.createWorkspace(c.ws)
			DeferCleanup(func() { kcp.cleanupWorkspaceTarget(testNs, c.ws) })
			DeferCleanup(func() { deleteClusterProvider(c.ws) })

			// The SAME namespace name (testNs) and the SAME resource name (shared) in every
			// workspace; only the value differs. A WatchRule watches its own namespace name on the
			// source cluster, so testNs is created inside each workspace (a distinct cluster).
			_, err := kcp.wsKubectl(hash, "create", "namespace", testNs)
			Expect(err).NotTo(HaveOccurred(), "create namespace in %s", c.ws)
			_, err = kcp.wsKubectl(hash, "-n", testNs, "create", "configmap", "shared",
				"--from-literal=which="+c.value)
			Expect(err).NotTo(HaveOccurred(), "create ConfigMap shared in %s", c.ws)

			writeKubeConfigSecret(c.ws+"-kubeconfig", "value", kcp.operatorKubeConfig(hash))
			_, err = applyClusterProvider(c.ws, c.ws+"-kubeconfig", "", testNs)
			Expect(err).NotTo(HaveOccurred())
			target := c.ws + "-target"
			_, err = applyGitTargetWithClusterProvider(testNs, target, providerName, c.folder, c.ws)
			Expect(err).NotTo(HaveOccurred())
			verifyResourceCondition("gittarget", target, testNs, "SourceClusterReachable", "True", "", "", "180s")

			rule := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata: {name: %s-rule, namespace: %s}
spec:
  targetRef: {kind: GitTarget, name: %s}
  rules:
  - resources: ["configmaps"]
`, c.ws, testNs, target)
			_, err = kubectlRunWithStdin(testNs, rule, "apply", "-f", "-")
			Expect(err).NotTo(HaveOccurred())
		}

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			for _, c := range cases {
				hit := findFileByBasename(filepath.Join(repo.CheckoutDir, c.folder), "shared.yaml")
				g.Expect(hit).NotTo(BeEmpty(), "expected shared mirrored under %s", c.folder)
				content, readErr := os.ReadFile(hit)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring("which: "+c.value),
					"folder %s must carry workspace %s's own value, not another workspace's",
					c.folder, c.ws)
			}
			// Generous: the initial snapshot writes each mirror promptly, but the ceiling also
			// covers one RequeueSteadyInterval (5m) periodic resync as a fallback.
		}).WithTimeout(420 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
	})
})
