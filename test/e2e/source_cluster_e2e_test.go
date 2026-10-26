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

// This file is the source-cluster corner for multi-cluster mirroring: a GitTarget names the
// cluster it mirrors FROM by referencing a namespaced KubeProvider (spec.kubeProviderRef). The
// KubeProvider is the home for that cluster's credential (spec.secretRef, a kubeconfig Secret in
// the provider's OWN namespace, or spec.inCluster where the installation allows it), for which
// other namespaces may use it (spec.allowedGitTargets), and for its audit route.
//
// Four kinds of spec live here:
//   - Input-validation specs (Scenario 1) assert the KubeProvider reconciler's Validated verdict on
//     bad kubeconfigs and on an inCluster provider the installation does not list, without a
//     remote cluster and without a dial.
//   - Reachability and admission specs (Scenarios 2-3) assert the GitTarget's projections for a
//     valid-but-unroutable provider, the install's own provider, a missing one, and one in another
//     tenant's namespace.
//   - Remote-mirror specs (Scenarios 4, 8, 9) mirror real REMOTE clusters — kcp WORKSPACES, cheap
//     logical clusters installed by Flux (test/e2e/setup/kcp, see kcp_workspace_test.go).
//     Scenario 8 holds the SAME namespace + resource with different content in three workspaces,
//     proving state is keyed by source cluster, not (namespace, GVR); Scenario 9 gives two
//     providers ONE name in two namespaces, and recreates one of them.
//   - The applier spec (Scenario 10) is an applier in this cluster applying into a remote one, as
//     gitops-api runs: Flux applies a folder into a workspace through Kustomization.spec.kubeConfig,
//     the GitTarget mirrors that workspace, and its FluxClaim holds the Kustomization through the
//     install's own provider. It shows the startup converge landing in the workspace, an edit made
//     there during an apply withheld from the apply's revert by the REMOTE object's managedFields,
//     and two saves that ask to catch up reporting the apply.
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

// writeKubeConfigSecret applies a Secret holding a kubeconfig under the given key, in namespace —
// the KubeProvider's own: its secretRef is resolved there, never from the source cluster.
func writeKubeConfigSecret(namespace, name, key, kubeconfig string) {
	GinkgoHelper()
	f, err := os.CreateTemp("", "e2e-kubeconfig-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.WriteString(kubeconfig)
	Expect(err).NotTo(HaveOccurred())
	Expect(f.Close()).To(Succeed())

	manifest, err := kubectlRunInNamespace(namespace, "create", "secret", "generic", name,
		"--from-file="+key+"="+f.Name(), "--dry-run=client", "-o", "yaml")
	Expect(err).NotTo(HaveOccurred(), "failed to render kubeconfig Secret manifest")
	_, err = kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply kubeconfig Secret")
}

// applyKubeProvider applies a KubeProvider in namespace whose secretRef names a kubeconfig Secret in
// that same namespace, letting its own namespace and the other namespaces listed mirror their own
// namespace. It returns the kubectl error so a spec can assert the apply succeeded (the CR is
// well-formed; the kubeconfig, not the CR, is what a validation case makes bad).
func applyKubeProvider(namespace, name, secretName, key string, admits ...string) (string, error) {
	keyLine := ""
	if key != "" {
		keyLine = "\n    key: " + key
	}
	grants := "\n  allowedGitTargets:\n    - consumerNamespaces: {from: Same}\n      sourceNamespaces: [\"{consumer.namespace}\"]"
	if len(admits) > 0 {
		grants += fmt.Sprintf(
			"\n    - consumerNamespaces: {from: Names, names: [%s]}\n      sourceNamespaces: [\"{consumer.namespace}\"]",
			strings.Join(admits, ", "),
		)
	}
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
metadata:
  name: %s
  namespace: %s
spec:
  secretRef:
    name: %s%s%s
`, name, namespace, secretName, keyLine, grants)
	return kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
}

// deleteKubeProvider removes a KubeProvider and waits until it is gone, so a recreate under the
// same name is a new object.
func deleteKubeProvider(namespace, name string) {
	_, _ = kubectlRunInNamespace(namespace, "delete", "kubeprovider", name, "--ignore-not-found")
}

// applyGitTargetWithKubeProvider applies a GitTarget in ns, on the GitBranch of gitProvider in
// branchNS (empty: ns), whose spec.kubeProviderRef names providerNS/provider (the source cluster it
// mirrors from).
func applyGitTargetWithKubeProvider(
	ns, name, gitProvider, branchNS, path, providerNS, provider string,
) (string, error) {
	manifest := ""
	if branchNS == "" {
		branchNS = ns
		// Applied only when missing: applying it again would drop a writer grant a spec added.
		if _, err := kubectlRunInNamespace(ns, "get", "gitbranch", gitBranchName(gitProvider, "main")); err != nil {
			manifest = gitBranchManifest(ns, gitProvider)
		}
	}
	manifest += fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: %s
  namespace: %s
spec:
  gitBranchRef:
    name: %s
    namespace: %s
  kubeProviderRef:
    name: %s
    namespace: %s
  path: %s
  commit:
    window:
      idleTimeout: "0s"
`, name, ns, gitBranchName(gitProvider, "main"), branchNS, provider, providerNS, path)
	return kubectlRunWithStdin(ns, manifest, "apply", "-f", "-")
}

// mirrorWorkspaceConfigMap stands up one remote mirror: a kcp workspace holding the ConfigMap name
// with which=value in the namespace sourceNS, a KubeProvider providerNS/provider whose Secret
// carries the workspace's kubeconfig, and a GitTarget plus WatchRule in targetNS mirroring that
// namespace into folder. The Secret, GitTarget and WatchRule follow the names
// kcpTunnel.cleanupWorkspaceTarget deletes.
//
//nolint:unparam // gitProvider is kept an explicit argument for readability; the corner uses one.
func mirrorWorkspaceConfigMap(
	kcp *kcpTunnel, ws, targetNS, providerNS, provider, gitProvider, branchNS, folder, name, value string,
) {
	GinkgoHelper()
	hash := kcp.createWorkspace(ws)
	// A WatchRule watches its OWN namespace name on the source cluster, so the ConfigMap must
	// live under that namespace IN the workspace — a separate cluster, so creating a namespace of
	// that name there is independent of the local one.
	_, err := kcp.wsKubectl(hash, "create", "namespace", targetNS)
	Expect(err).NotTo(HaveOccurred(), "create watched namespace in %s", ws)
	_, err = kcp.wsKubectl(hash, "-n", targetNS, "create", "configmap", name, "--from-literal=which="+value)
	Expect(err).NotTo(HaveOccurred(), "create ConfigMap %s in %s", name, ws)

	writeKubeConfigSecret(providerNS, ws+"-kubeconfig", "value", kcp.operatorKubeConfig(hash))
	_, err = applyKubeProvider(providerNS, provider, ws+"-kubeconfig", "")
	Expect(err).NotTo(HaveOccurred())
	target := ws + "-target"
	_, err = applyGitTargetWithKubeProvider(targetNS, target, gitProvider, branchNS, folder, providerNS, provider)
	Expect(err).NotTo(HaveOccurred())
	verifyResourceCondition("gittarget", target, targetNS, "SourceClusterReachable", "True", "", "", "180s")

	rule := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata: {name: %s-rule, namespace: %s}
spec:
  gitTargetRef: {name: %s}
  rules:
  - resources: ["configmaps"]
`, ws, targetNS, target)
	_, err = kubectlRunWithStdin(targetNS, rule, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
}

// expectMirroredValue asserts that folder holds name.yaml carrying which=value.
func expectMirroredValue(g Gomega, checkoutDir, folder, name, value string) {
	hit := findFileByBasename(filepath.Join(checkoutDir, folder), name+".yaml")
	g.Expect(hit).NotTo(BeEmpty(), "expected %s mirrored under %s", name, folder)
	content, readErr := os.ReadFile(hit)
	g.Expect(readErr).NotTo(HaveOccurred())
	g.Expect(string(content)).To(ContainSubstring("which: "+value),
		"folder %s must carry its own source cluster's value", folder)
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

var _ = Describe("Manager source cluster / KubeProvider", Label("source-cluster"), Ordered, func() {
	const providerName = "sc-provider"

	var (
		testNs string
		// tenantNs is a second tenant: it writes through testNs's GitBranch (admitted by the
		// GitProvider) and is admitted by testNs's KubeProviders only where a spec says so.
		tenantNs string
		repo     *RepoArtifacts
		// kcp is the workspace harness for the remote-mirror specs. It is nil when kcp is not
		// installed (a default `task test-e2e` run) — those specs then Skip rather than fail.
		kcp *kcpTunnel
	)

	BeforeAll(func() {
		skipUnlessSourceClusterEnabled()

		testNs = testNamespaceFor("source-cluster")
		tenantNs = testNamespaceFor("source-cluster-tenant")
		_, _ = kubectlRun("create", "namespace", testNs)
		_, _ = kubectlRun("create", "namespace", tenantNs)

		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-source-cluster-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply repo secrets")

		createReadyGitProvider(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		// The tenant writes through this namespace's GitBranch, so the GitBranch admits it; which
		// KubeProvider it may READ through is what the specs below decide.
		_, err = kubectlRunWithStdin(testNs, gitBranchManifest(testNs, providerName, tenantNs), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to admit the tenant namespace to the GitBranch")

		if kcpAvailable() {
			kcp = startKcpTunnel()
		}
	})

	AfterAll(func() {
		if kcp != nil {
			kcp.stop()
		}
		cleanupNamespace(tenantNs)
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(60 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// Scenario 1 — the KubeProvider reconciler's credential validation is legible, and never dials.
	inputCases := []struct {
		name   string
		reason string
		setup  func(provider string) (secretName, key string)
	}{
		{
			name:   "a missing Secret",
			reason: "KubeConfigSecretNotFound",
			setup:  func(_ string) (string, string) { return "sc-absent", "" },
		},
		{
			name:   "a missing key",
			reason: "KubeConfigKeyNotFound",
			setup: func(p string) (string, string) {
				writeKubeConfigSecret(testNs, p+"-kc", "somewhere-else", rawKubeConfigWithServer(unreachableAPIServer))
				return p + "-kc", "value"
			},
		},
		{
			name:   "an unparseable kubeconfig",
			reason: "KubeConfigInvalid",
			setup: func(p string) (string, string) {
				writeKubeConfigSecret(testNs, p+"-kc", "value", "this is not a kubeconfig")
				return p + "-kc", ""
			},
		},
		{
			name:   "an exec auth provider",
			reason: "KubeConfigExecNotAllowed",
			setup: func(p string) (string, string) {
				writeKubeConfigSecret(testNs, p+"-kc", "value", execKubeConfig())
				return p + "-kc", ""
			},
		},
		{
			name:   "insecure TLS",
			reason: "KubeConfigInsecureTLSNotAllowed",
			setup: func(p string) (string, string) {
				writeKubeConfigSecret(testNs, p+"-kc", "value", insecureKubeConfig())
				return p + "-kc", ""
			},
		},
		{
			name:   "a file-path credential",
			reason: "KubeConfigFileReferenceNotAllowed",
			setup: func(p string) (string, string) {
				writeKubeConfigSecret(testNs, p+"-kc", "value", fileReferenceKubeConfig())
				return p + "-kc", ""
			},
		},
		{
			// The operator's own namespace holds a Secret of this name; the provider's does not, and
			// a Secret is only ever read from the provider's own namespace.
			name:   "a Secret that exists only in another namespace",
			reason: "KubeConfigSecretNotFound",
			setup: func(p string) (string, string) {
				writeKubeConfigSecret(resolveE2ENamespace(), p+"-kc", "value",
					rawKubeConfigWithServer(unreachableAPIServer))
				DeferCleanup(func() {
					_, _ = kubectlRunInNamespace(
						resolveE2ENamespace(),
						"delete",
						"secret",
						p+"-kc",
						"--ignore-not-found",
					)
				})
				return p + "-kc", ""
			},
		},
	}
	for i, tc := range inputCases {
		It("fails KubeProvider Validated (no dial) for "+tc.name+" with reason "+tc.reason, func() {
			name := fmt.Sprintf("sc-input-%d", i)
			secretName, key := tc.setup(name)
			DeferCleanup(func() { deleteKubeProvider(testNs, name) })
			// The KubeProvider CR is well-formed (the kubeconfig is bad, not the CR), so the
			// apply itself must succeed — the controller then reports the typed reason on Validated.
			_, err := applyKubeProvider(testNs, name, secretName, key)
			Expect(err).NotTo(HaveOccurred())
			verifyResourceCondition("kubeprovider", name, testNs, "Validated", "False", tc.reason, "")
		})
	}

	// Scenario 1b — the operator's own identity needs the installation's consent. A KubeProvider in a
	// tenant namespace that sets inCluster is not on --in-cluster-kube-providers, so it is never
	// used: it stalls, and a GitTarget mirroring through it never reaches an API server.
	It("never uses the operator's identity for an inCluster provider the installation does not list", func() {
		const name = "sc-own-identity"
		DeferCleanup(func() { deleteKubeProvider(testNs, name) })
		_, err := kubectlRunWithStdin(testNs, fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
metadata: {name: %s, namespace: %s}
spec:
  inCluster: {}
  allowedGitTargets:
    - consumerNamespaces: {from: Same}
      sourceNamespaces: ["{consumer.namespace}"]
`, name, testNs), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("kubeprovider", name, testNs, "Validated", "False", "InClusterNotAuthorized", "")
		verifyResourceCondition("kubeprovider", name, testNs, "Stalled", "True", "InClusterNotAuthorized", "")

		target := "sc-own-identity-target"
		_, err = applyGitTargetWithKubeProvider(testNs, target, providerName, "", "clusters/own", testNs, name)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("gittarget", target, testNs, "KubeProviderReady", "False", "KubeProviderNotReady",
			"--in-cluster-kube-providers")
		verifyResourceCondition("gittarget", target, testNs,
			"SourceClusterReachable", "False", "InClusterNotAuthorized", "", "150s")
	})

	// Scenario 2 — a valid kubeconfig that cannot be dialed: the KubeProvider is Validated=True,
	// and the GitTarget that references it projects SourceClusterReachable=False.
	It("separates Validated (inputs) from SourceClusterReachable (runtime)", func() {
		const name = "sc-unreachable"
		writeKubeConfigSecret(testNs, name+"-kc", "value", rawKubeConfigWithServer(unreachableAPIServer))
		_, err := applyKubeProvider(testNs, name, name+"-kc", "")
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("kubeprovider", name, testNs, "Validated", "True", "Succeeded", "")

		target := "sc-unreachable-target"
		_, err = applyGitTargetWithKubeProvider(testNs, target, providerName, "", "clusters/unreachable", testNs, name)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("gittarget", target, testNs, "Validated", "True", "Succeeded", "")
		verifyResourceCondition("gittarget", target, testNs,
			"SourceClusterReachable", "False", "SourceClusterUnreachable", "", "150s")
	})

	// Scenario 2b — a tenant's KubeProvider is the tenant's. Another namespace may not mirror
	// through it until a grant in its allowedGitTargets admits that namespace.
	It("refuses a second tenant a KubeProvider whose grants do not admit it", func() {
		const name = "sc-unreachable"
		target := "sc-tenant-target"
		_, err := applyGitTargetWithKubeProvider(
			tenantNs,
			target,
			providerName,
			testNs,
			"clusters/tenant",
			testNs,
			name,
		)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("gittarget", target, tenantNs, "Validated", "False", "NamespaceNotAuthorized",
			"KubeProvider "+testNs+"/"+name)

		_, err = applyKubeProvider(testNs, name, name+"-kc", "", tenantNs)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("gittarget", target, tenantNs, "Validated", "True", "Succeeded", "")
	})

	// Scenario 3 — the install's own provider: "local" in the operator's namespace mirrors the
	// operator's own cluster, and admits a GitTarget in any namespace through the suite's grant.
	It("mirrors the operator's own cluster through the install's KubeProvider", func() {
		target := "sc-local-target"
		_, err := applyGitTargetWithKubeProvider(testNs, target, providerName, "", "clusters/local",
			resolveE2ENamespace(), e2eKubeProviderName)
		Expect(err).NotTo(HaveOccurred())

		verifyResourceCondition("gittarget", target, testNs,
			"SourceClusterReachable", "True", "LocalCluster", "")
	})

	// Scenario 3b — the hard gate: a GitTarget that references a KubeProvider which does NOT exist
	// is held NotReady (Validated=False, KubeProviderNotFound) and never mirrors. Nothing falls back
	// to an implicit local identity, and the operator never creates the object to rescue it.
	It("holds a GitTarget NotReady when its KubeProvider does not exist (no bypass)", func() {
		target := "sc-missing-provider-target"
		_, err := applyGitTargetWithKubeProvider(testNs, target, providerName, "", "clusters/missing",
			testNs, "sc-does-not-exist")
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("gittarget", target, testNs,
			"Validated", "False", "KubeProviderNotFound", "")
	})

	// Scenario 4 — a REAL remote cluster: a tenant namespace mirrors a ConfigMap out of one kcp
	// workspace through its own KubeProvider and Secret. Proves the whole remote path end to end
	// (KubeProvider resolver -> per-cluster clientContext -> per-cluster discovery -> per-cluster
	// watch -> target-scoped writer) against an actually-remote API.
	It("mirrors a ConfigMap from a kcp workspace", func() {
		if kcp == nil {
			Skip("kcp is not installed; run this corner via `task test-e2e-source-cluster`")
		}
		const ws = "sc-mirror"
		DeferCleanup(func() { kcp.cleanupWorkspaceTarget(testNs, ws) })
		DeferCleanup(func() { deleteKubeProvider(testNs, ws) })
		mirrorWorkspaceConfigMap(
			kcp,
			ws,
			testNs,
			testNs,
			ws,
			providerName,
			"",
			"clusters/kcp",
			"sc-remote-cm",
			"from-kcp",
		)
		waitForStreamsRunning(ws+"-target", testNs)

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			expectMirroredValue(g, repo.CheckoutDir, "clusters/kcp", "sc-remote-cm", "from-kcp")
		}).WithTimeout(180 * time.Second).Should(Succeed())
	})

	// Scenario 8 — source-cluster identity is load-bearing. Three workspaces each hold the SAME
	// namespace + the SAME resource name (ConfigMap "shared") with DIFFERENT content, mirrored by
	// three GitTargets (each naming its own KubeProvider) into three folders. If the operator keyed
	// state by (namespace, GVR) alone the three identical identities would collapse into one; that
	// they land as three distinct files, each carrying its own workspace's value, proves everything
	// is keyed by SOURCE CLUSTER.
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
			DeferCleanup(func() { kcp.cleanupWorkspaceTarget(testNs, c.ws) })
			DeferCleanup(func() { deleteKubeProvider(testNs, c.ws) })
			mirrorWorkspaceConfigMap(kcp, c.ws, testNs, testNs, c.ws, providerName, "", c.folder, "shared", c.value)
		}

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			for _, c := range cases {
				expectMirroredValue(g, repo.CheckoutDir, c.folder, "shared", c.value)
			}
			// Generous: the initial snapshot writes each mirror promptly, but the ceiling also
			// covers one RequeueSteadyInterval (5m) periodic resync as a fallback.
		}).WithTimeout(420 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
	})

	// Scenario 9 — two KubeProviders with ONE name in two namespaces are two source clusters: their
	// watches, caches and audit routes are keyed by namespace/name, so each folder carries its own
	// workspace's value. Deleting one and recreating it under the same name against a third
	// workspace must start a new connection rather than reuse the old one's clients or catalog, and
	// must leave the other namespace's mirror alone.
	It("keeps two same-named KubeProviders in two namespaces apart, also across a recreate", func() {
		if kcp == nil {
			Skip("kcp is not installed; run this corner via `task test-e2e-source-cluster`")
		}
		const provider = "sc-twin"
		DeferCleanup(func() { kcp.cleanupWorkspaceTarget(testNs, "sc-twin-a") })
		DeferCleanup(func() { kcp.cleanupWorkspaceTarget(tenantNs, "sc-twin-b") })
		DeferCleanup(func() { deleteKubeProvider(testNs, provider) })
		DeferCleanup(func() { deleteKubeProvider(tenantNs, provider) })

		mirrorWorkspaceConfigMap(kcp, "sc-twin-a", testNs, testNs, provider, providerName, "",
			"clusters/twin-a", "twin", "alpha")
		mirrorWorkspaceConfigMap(kcp, "sc-twin-b", tenantNs, tenantNs, provider, providerName, testNs,
			"clusters/twin-b", "twin", "beta")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			expectMirroredValue(g, repo.CheckoutDir, "clusters/twin-a", "twin", "alpha")
			expectMirroredValue(g, repo.CheckoutDir, "clusters/twin-b", "twin", "beta")
		}).WithTimeout(300 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
		verifyResourceCondition("kubeprovider", provider, testNs, "Ready", "True", "", "")
		verifyResourceCondition("kubeprovider", provider, tenantNs, "Ready", "True", "", "")

		By("recreating the tenant's provider under the same name against another workspace")
		DeferCleanup(func() { kcp.deleteWorkspace("sc-twin-c") })
		hash := kcp.createWorkspace("sc-twin-c")
		_, err := kcp.wsKubectl(hash, "create", "namespace", tenantNs)
		Expect(err).NotTo(HaveOccurred())
		_, err = kcp.wsKubectl(hash, "-n", tenantNs, "create", "configmap", "twin", "--from-literal=which=gamma")
		Expect(err).NotTo(HaveOccurred())
		deleteKubeProvider(tenantNs, provider)
		writeKubeConfigSecret(tenantNs, "sc-twin-c-kubeconfig", "value", kcp.operatorKubeConfig(hash))
		DeferCleanup(func() {
			_, _ = kubectlRunInNamespace(tenantNs, "delete", "secret", "sc-twin-c-kubeconfig", "--ignore-not-found")
		})
		_, err = applyKubeProvider(tenantNs, provider, "sc-twin-c-kubeconfig", "")
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			expectMirroredValue(g, repo.CheckoutDir, "clusters/twin-b", "twin", "gamma")
			expectMirroredValue(g, repo.CheckoutDir, "clusters/twin-a", "twin", "alpha")
		}).WithTimeout(420 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
	})

	// Scenario 10 — an applier in this cluster applying into the remote one: Flux applies a folder
	// into a workspace through Kustomization.spec.kubeConfig, the GitTarget mirrors that workspace
	// through a secretRef KubeProvider, and the FluxClaim holds the Kustomization through the
	// install's own provider. So the apply window judges the managedFields of the REMOTE object. It
	// has its own repository, so no GitTarget of the scenarios above shares its branch.
	//
	// The e2e kcp has no audit stream, so no write in the workspace names its author: everything is
	// mirrored from the watch, and every event waits out the attribution grace first.
	Describe("Flux applying into a workspace the GitTarget mirrors", Ordered, func() {
		const ws = "sc-flux"
		var (
			p      *timingPipeline
			r      *echoRun
			ksName string
		)

		BeforeAll(func() {
			if kcp == nil {
				Skip("kcp is not installed; run this corner via `task test-e2e-source-cluster`")
			}
			grantAppliersInAnyNamespace()
			ensurePrometheusClient()
			fluxRepo := SetupRepo(resolveE2EContext(), testNs,
				fmt.Sprintf("e2e-source-cluster-flux-%d", GinkgoRandomSeed()))
			p = newTimingPipeline(testNs, ws, fluxRepo)
			ksName = ws + "-" + testNs
			_, err := kubectlRunInNamespace(testNs, "apply", "-f", fluxRepo.SecretsYAML)
			Expect(err).NotTo(HaveOccurred(), "failed to apply repo secrets")
			configureRepoOriginWithCredentials(fluxRepo, testNs)

			By("provisioning the workspace, and the namespace the folder holds inside it")
			DeferCleanup(kcp.deleteWorkspace, ws)
			hash := kcp.createWorkspace(ws)
			_, err = kcp.wsKubectl(hash, "create", "namespace", testNs)
			Expect(err).NotTo(HaveOccurred(), "create the folder's namespace in %s", ws)

			By("mirroring the workspace: KubeProvider, GitProvider with a receiver, GitTarget, WatchRule")
			DeferCleanup(func() {
				deleteKubeProvider(testNs, ws)
				cleanupNamespacedResource(testNs, "secret", ws+"-kubeconfig")
			})
			writeKubeConfigSecret(testNs, ws+"-kubeconfig", "value", kcp.operatorKubeConfig(hash))
			_, err = applyKubeProvider(testNs, ws, ws+"-kubeconfig", "")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.cleanup)
			createReadyGitProvider(p.providerName, testNs, fluxRepo.GitSecretHTTP, fluxRepo.RepoURLHTTP)
			p.hookID = registerOperatorReceiver(testNs, p.receiverName, fluxRepo.RepoName)
			_, err = applyGitTargetWithKubeProvider(testNs, p.targetName, p.providerName, "", p.gitPath, testNs, ws)
			Expect(err).NotTo(HaveOccurred())
			verifyResourceCondition("gittarget", p.targetName, testNs, "SourceClusterReachable", "True", "", "", "180s")
			p.applyWatchRule()
			waitForStreamsRunning(p.targetName, testNs)

			r = &echoRun{
				timingPipeline: p,
				applier: echoApplier{
					name: "Flux",
					owns: func(g Gomega, ns, configMap string) bool {
						out, err := kcp.wsKubectl(hash, "-n", ns, "get", "configmap", configMap,
							`-o=jsonpath={.metadata.labels.kustomize\.toolkit\.fluxcd\.io/name}`)
						g.Expect(err).NotTo(HaveOccurred())
						return strings.TrimSpace(out) == ksName
					},
					operation: func(g Gomega) claimOperation {
						return readClaimOperation(g, testNs, fluxClaimResource, ksName)
					},
				},
				kubectl: func(args ...string) (string, error) {
					return kcp.wsKubectl(hash, append([]string{"-n", testNs}, args...)...)
				},
			}

			By("creating a ConfigMap in the workspace, and waiting until the GitTarget mirrored it")
			_, err = r.liveKubectl("create", "configmap", timingConfigMap, "--from-literal=value=seed")
			Expect(err).NotTo(HaveOccurred())
			_, err = r.liveKubectl("label", "configmap", timingConfigMap, timingLabelKey+"=true")
			Expect(err).NotTo(HaveOccurred())
			r.eventuallyEverywhere(timingConfigMap, "seed", echoApplyBudget)

			By("creating the Kustomization, which applies the folder into the workspace once")
			DeferCleanup(func() {
				cleanupNamespacedResource("flux-system", "kustomization", ksName)
				cleanupNamespacedResource("flux-system", "gitrepository", ksName)
				cleanupNamespacedResource("flux-system", "secret", ksName+"-git")
				cleanupNamespacedResource("flux-system", "secret", ksName+"-kubeconfig")
			})
			applyFluxIntoWorkspace(testNs, fluxRepo, ksName, kcp.operatorKubeConfig(hash), "./"+p.gitPath)

			By("pushing a Git-side change the Kustomization has not fetched, then holding it with a FluxClaim")
			r.pushFromOutside("converged", "c1")
			DeferCleanup(cleanupNamespacedResource, testNs, fluxClaimResource, ksName)
			createFluxClaim(testNs, ksName, ksName)
			nameApplierClaim(testNs, p.targetName, "FluxClaim", ksName, "")
		})

		// Naming the FluxClaim converges it once: a CatchUp of the tip, which holds a ConfigMap only
		// Git has, since the GitRepository's 24h interval never fetched it. So the ConfigMap reaches
		// the workspace through the FluxClaim's apply alone, and Flux's labels on it say who applied it.
		It("applies its startup converge into the workspace, and mirrors what it applied", func() {
			verifyResourceCondition(fluxClaimResource, ksName, testNs, "Configured", "True", "", "")
			verifyResourceCondition(fluxClaimResource, ksName, testNs, "Ready", "True", "Succeeded", "")
			verifyResourceCondition("gittarget", p.targetName, testNs, "ApplierClaimReady", "True", "Succeeded", "")
			Eventually(func(g Gomega) {
				head := p.remoteBranchHead(g)
				g.Expect(fluxLastAppliedRevision(g, ksName)).To(ContainSubstring(head))
				done := r.applier.operation(g)
				g.Expect(done.Reason).To(Equal("CatchUp"))
				g.Expect(done.Commit).To(Equal(head))
				g.Expect(done.Phase).To(Equal("Succeeded"))
				live, exists := r.liveValue(g, "converged")
				g.Expect(exists).To(BeTrue(), "the converge applies the Git-side change into the workspace")
				g.Expect(live).To(Equal("c1"))
				g.Expect(r.applier.owns(g, testNs, "converged")).To(BeTrue(), "Flux applied it")
				g.Expect(r.applier.owns(g, testNs, timingConfigMap)).To(BeTrue(), "Flux adopted the mirrored one")
			}, echoApplyBudget, 2*time.Second).Should(Succeed())
			verifyResourceCondition(fluxClaimResource, ksName, testNs, "InControl", "True", "Suspended", "")
			r.consistentlyEverywhere("converged", "c1")
			r.consistentlyEverywhere(timingConfigMap, "seed")
		})

		// The collision the apply window exists for, as E5 provokes it: an edit through the workspace's
		// API while an apply runs. The apply reverts it in the workspace; the window withholds that
		// write, because the object's managedFields name kustomize-controller alone, after the edit.
		It("withholds an apply that reverts an edit made in the workspace during it", func() {
			withheld := fmt.Sprintf(`sum(gitopsreverser_apply_window_collisions_total{gitbranch_namespace=%q,`+
				`gitbranch_name=%q,outcome="withheld"}) or vector(0)`, testNs, gitBranchName(p.providerName, "main"))
			before, err := queryPrometheus(withheld)
			Expect(err).NotTo(HaveOccurred())

			r.editDuringApply()
			waitForMetricWithTimeout(withheld, func(v float64) bool { return v > before },
				"the apply over the workspace edit is counted as a withheld collision", time.Minute)
		})

		// A save on the workspace's GitTarget that asks to catch up is applied once it is pushed, and
		// each request reports the apply: Applied=True, with the revision Flux applied. Two saves made
		// together may share one apply.
		//
		// The saves record empty commits (whenNothingToCommit: CommitEmpty). A request's author is
		// captured at admission in this cluster, and with no audit stream from kcp a write in the
		// workspace names no author, so a request never attaches to that write's window.
		It("applies two saves that ask to catch up, and reports the apply on each", func() {
			names := []string{"sc-flux-save-1", "sc-flux-save-2"}
			for _, name := range names {
				applyCatchUpSave(testNs, name, p.targetName)
			}
			commits := map[string]string{}
			for _, name := range names {
				Eventually(func(g Gomega) {
					g.Expect(commitRequestCondition(g, testNs, name, "Ready")).To(Equal("True"))
					g.Expect(commitRequestField(g, testNs, name, `{.status.conditions[?(@.type=="Ready")].reason}`)).
						To(Equal("NoWindow"))
					g.Expect(commitRequestCondition(g, testNs, name, "Pushed")).To(Equal("True"))
					commits[name] = commitRequestField(g, testNs, name, "{.status.commit}")
					g.Expect(commits[name]).NotTo(BeEmpty())
				}, 2*time.Minute, 2*time.Second).Should(Succeed())
			}

			last := commits[names[len(names)-1]]
			Eventually(func(g Gomega) {
				for _, name := range names {
					applied := commitRequestCondition(g, testNs, name, "Applied")
					reason := commitRequestField(g, testNs, name, `{.status.conditions[?(@.type=="Applied")].reason}`)
					if applied == "False" {
						StopTrying(fmt.Sprintf("CommitRequest %s is Applied=False (%s): only a commit the branch "+
							"lost may be", name, reason)).Now()
					}
					g.Expect(applied).To(Equal("True"), "CommitRequest %s Applied (%s)", name, reason)
					g.Expect(reason).To(Equal("Applied"))
					// A later save's commit holds the earlier one's, so one apply may settle both.
					g.Expect(commitRequestField(g, testNs, name, "{.status.appliedRevision}")).
						To(Or(ContainSubstring(commits[name]), ContainSubstring(last)))
				}
				g.Expect(commitRequestField(g, testNs, names[len(names)-1], "{.status.appliedRevision}")).
					To(ContainSubstring(last))
				g.Expect(fluxLastAppliedRevision(g, ksName)).To(ContainSubstring(last))
			}, echoApplyBudget, 2*time.Second).Should(Succeed())
		})
	})
})

// applyFluxIntoWorkspace creates, in flux-system, a GitRepository following main and a Kustomization
// applying path from it into a kcp workspace: its spec.kubeConfig names a Secret holding kubeconfig
// under the key "value", which kustomize-controller, in this cluster, dials. Both are on a 24h
// interval, so a later commit is applied only when the FluxClaim asks. It waits for the first apply,
// made before any applier claim holds the Kustomization.
func applyFluxIntoWorkspace(testNs string, repo *RepoArtifacts, name, kubeconfig, path string) {
	GinkgoHelper()
	writeKubeConfigSecret("flux-system", name+"-kubeconfig", "value", kubeconfig)
	fields := struct {
		Namespace, SecretName, Name, RepoURL, Branch, Interval, Username, Password string
	}{
		Namespace: "flux-system", SecretName: name + "-git", Name: name, RepoURL: repo.RepoURLHTTP,
		Branch: "main", Interval: "24h",
	}
	checkout := gitCheckout{secretNamespace: testNs, gitSecretName: repo.GitSecretHTTP}
	fields.Username, fields.Password = checkout.readGitCredentialSecretDataBase64()
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/flux-gitrepository-http.tmpl", fields,
		"flux-system")).To(Succeed(), "failed to apply the Flux GitRepository")
	_, err := kubectlRunWithStdin("flux-system", fmt.Sprintf(`apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: flux-system
spec:
  interval: 24h
  timeout: 2m
  path: %s
  prune: true
  sourceRef:
    kind: GitRepository
    name: %s
  kubeConfig:
    secretRef:
      name: %s-kubeconfig
      key: value
`, name, path, name, name), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply the Flux Kustomization")
	// Ready's message is where an unreachable workspace shows: a TLS or DNS failure to the front-proxy.
	Eventually(func(g Gomega) {
		out, err := kubectlRunInNamespace("flux-system", "get", "kustomization", name, "-o",
			`jsonpath={.status.conditions[?(@.type=="Ready")].status}|`+
				`{.status.conditions[?(@.type=="Ready")].message}`)
		g.Expect(err).NotTo(HaveOccurred())
		status, message, _ := strings.Cut(strings.TrimSpace(out), "|")
		g.Expect(status).To(Equal("True"), "Kustomization %s is not Ready: %s", name, message)
	}, 3*time.Minute, 2*time.Second).Should(Succeed(), "the Kustomization must apply its folder into the workspace")
}

// applyCatchUpSave creates the CommitRequest name on gitTarget, asking its applier to catch up once
// it is pushed. It attaches only to a window already open, and records its message in an empty
// commit when there is none.
func applyCatchUpSave(namespace, name, gitTarget string) {
	GinkgoHelper()
	_, err := kubectlRunWithStdin(namespace, fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: CommitRequest
metadata:
  name: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  message: "e2e: save %s and apply it"
  catchUp: AfterPush
  whenNothingToCommit: CommitEmpty
  window:
    attachTimeout: "0s"
`, name, namespace, gitTarget, name), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to create CommitRequest %s", name)
}
