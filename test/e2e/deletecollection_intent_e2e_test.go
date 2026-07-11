// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// DeleteCollection intent & attribution proves two things together against the
// running operator:
//
//  1. Deletion-as-intent: an object marked with a deletionTimestamp is removed from
//     the Git intent tree immediately — at delete-request time — even when a finalizer
//     keeps it Terminating in the cluster. Later finalizer cleanup and the eventual
//     DELETED produce no further Git change.
//  2. DeleteCollection attribution: a name-less collection delete is attributed
//     per object via the response-body expander, so each removal commit is authored by
//     the actor who ran the delete — not the committer, and not whoever clears the
//     finalizer.
//
// The state correctness of collection deletes is solved by construction in watch-first
// (one watch event per object); this spec adds the intent semantics and the attribution
// that the headline claim depends on. See
// docs/spec/deletecollection-attribution-expander.md.
//
// Not Serial: the GitProvider uses a 0s commit window, so every watched event commits
// immediately as its own commit, and every assertion reads the author/state scoped to a
// unique file path, so concurrent audit traffic from other specs cannot interfere. Each
// It scopes its collection delete with a per-spec label selector so the specs are
// isolated even though they share one namespace.
var _ = Describe("DeleteCollection intent & attribution", Label("manager"), Ordered, func() {
	var (
		testNs        string
		repo          *RepoArtifacts
		gitProvName   string
		gitTargetName string
		watchRuleName string
		alice         *kubernetes.Clientset
		cleanupBot    *kubernetes.Clientset
	)

	BeforeAll(func() {
		if configuredAuthorModeEnabled() {
			Skip("watch-first configured-author mode has no audit facts for delete attribution")
		}

		By("creating deletecollection-intent test namespace")
		testNs = testNamespaceFor("dc-intent")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up the Gitea repo and credentials")
		repo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-dc-intent-%d", GinkgoRandomSeed()),
		)
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("setting up GitProvider (0s commit window), GitTarget and WatchRule")
		seed := GinkgoRandomSeed()
		gitProvName = fmt.Sprintf("dc-intent-gitprovider-%d", seed)
		gitTargetName = fmt.Sprintf("dc-intent-gittarget-%d", seed)
		watchRuleName = fmt.Sprintf("dc-intent-watchrule-%d", seed)

		createReadyGitProvider(gitProvName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)

		createValidatedGitTarget(gitTargetName, testNs, gitProvName, dcIntentBasePath)

		data := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{Name: watchRuleName, Namespace: testNs, DestinationName: gitTargetName}
		err = applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", data, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply WatchRule")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		// Attribution flows only once the configmaps type is live; a write made while
		// the type is still building its first checkpoint would land unattributed.
		waitForStreamsRunning(gitTargetName, testNs)

		By("building impersonated clients for the actor and a separate finalizer-clearing identity")
		alice, err = impersonatedConfigMapClient("oidc-alice", "Alice Liddell", "alice@configbutler.ai")
		Expect(err).NotTo(HaveOccurred(), "failed to build alice client")
		// cleanupBot is a DISTINCT identity: it stands in for the finalizer controller
		// that completes deletion. It must never become the author of a removal.
		cleanupBot, err = impersonatedConfigMapClient("oidc-cleanup", "Cleanup Bot", "cleanup@configbutler.ai")
		Expect(err).NotTo(HaveOccurred(), "failed to build cleanupBot client")
	})

	AfterAll(func() {
		// Clear any lingering finalizers so the namespace can be torn down.
		forceClearConfigMapFinalizers(cleanupBot, testNs)
		cleanupPipeline(testNs, gitProvName, gitTargetName, watchRuleName)
		cleanupNamespace(testNs)
	})

	It("removes every collection member and attributes each removal to the actor", func() {
		label := fmt.Sprintf("dctest-removeall-%d", GinkgoRandomSeed())
		names := []string{label + "-a", label + "-b", label + "-c"}

		By("creating the configmaps and waiting for them in Git")
		for _, n := range names {
			Expect(createLabeledConfigMap(alice, testNs, n, label, nil)).To(Succeed())
		}
		for _, n := range names {
			waitForFilePresent(repo, configMapRepoPath(testNs, n))
		}

		By("running a collection delete as the actor")
		Expect(deleteConfigMapCollection(alice, testNs, "dctest="+label)).To(Succeed())

		By("asserting every file is removed and authored by the actor")
		for _, n := range names {
			waitForFileDeletedByActor(repo, configMapRepoPath(testNs, n))
		}
	})

	It("removes a finalizer object at intent time, authored by the actor, while it is still Terminating", func() {
		label := fmt.Sprintf("dctest-finalizer-%d", GinkgoRandomSeed())
		plain := label + "-plain"
		stuck := label + "-stuck"

		By("creating one plain and one finalizer-guarded configmap")
		Expect(createLabeledConfigMap(alice, testNs, plain, label, nil)).To(Succeed())
		Expect(createLabeledConfigMap(alice, testNs, stuck, label, []string{"example.com/cleanup"})).To(Succeed())
		waitForFilePresent(repo, configMapRepoPath(testNs, plain))
		waitForFilePresent(repo, configMapRepoPath(testNs, stuck))

		By("running a collection delete as the actor")
		Expect(deleteConfigMapCollection(alice, testNs, "dctest="+label)).To(Succeed())

		stuckPath := configMapRepoPath(testNs, stuck)
		By("asserting both files are removed at intent time and authored by the actor")
		waitForFileDeletedByActor(repo, configMapRepoPath(testNs, plain))
		waitForFileDeletedByActor(repo, stuckPath)

		By("confirming the finalizer object is still Terminating in the cluster (removed from Git, not from the API)")
		ts, err := kubectlRunInNamespace(testNs, "get", "configmap", stuck,
			"-o", "jsonpath={.metadata.deletionTimestamp}")
		Expect(err).NotTo(HaveOccurred(), "the finalizer object must still exist in the cluster")
		Expect(strings.TrimSpace(ts)).NotTo(BeEmpty(), "the object should carry a deletionTimestamp (Terminating)")

		removalHash := commitHashForPath(repo, stuckPath)

		By("clearing the finalizer as a DIFFERENT identity (the stand-in finalizer controller)")
		Expect(removeConfigMapFinalizers(cleanupBot, testNs, stuck)).To(Succeed())

		By("waiting for the object to actually leave the API after finalization")
		Eventually(func(g Gomega) {
			_, getErr := kubectlRunInNamespace(testNs, "get", "configmap", stuck)
			g.Expect(getErr).To(HaveOccurred(), "the object should be gone from the API once finalizers clear")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("asserting the finalizer clearing and eventual DELETED produced NO new commit (folded to no-op)")
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			g.Expect(commitHashForPath(repo, stuckPath)).To(Equal(removalHash),
				"the removal commit (authored by the actor) must remain the last commit for the path")
		}, 8*time.Second, 2*time.Second).Should(Succeed())
	})

	It("removes a single finalizer object at intent time too (the rule is not collection-specific)", func() {
		label := fmt.Sprintf("dctest-single-%d", GinkgoRandomSeed())
		name := label + "-foo"

		By("creating a single finalizer-guarded configmap")
		Expect(createLabeledConfigMap(alice, testNs, name, label, []string{"example.com/cleanup"})).To(Succeed())
		repoPath := configMapRepoPath(testNs, name)
		waitForFilePresent(repo, repoPath)

		By("deleting the single object by name as the actor")
		Expect(alice.CoreV1().ConfigMaps(testNs).Delete(
			context.Background(), name, metav1.DeleteOptions{})).To(Succeed())

		By("asserting the file is removed at intent time and authored by the actor")
		waitForFileDeletedByActor(repo, repoPath)

		By("confirming the object is still Terminating in the cluster")
		ts, err := kubectlRunInNamespace(testNs, "get", "configmap", name,
			"-o", "jsonpath={.metadata.deletionTimestamp}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(ts)).NotTo(BeEmpty())

		By("clearing the finalizer to let it finalize")
		Expect(removeConfigMapFinalizers(cleanupBot, testNs, name)).To(Succeed())
	})

	It("scopes a label-selector collection delete to matching objects and leaves siblings", func() {
		label := fmt.Sprintf("dctest-selector-%d", GinkgoRandomSeed())
		matchA := label + "-match-a"
		matchB := label + "-match-b"
		sibling := label + "-sibling"

		By("creating two matching configmaps and one non-matching sibling")
		Expect(createLabeledConfigMap(alice, testNs, matchA, label, nil, "tier", "doomed")).To(Succeed())
		Expect(createLabeledConfigMap(alice, testNs, matchB, label, nil, "tier", "doomed")).To(Succeed())
		Expect(createLabeledConfigMap(alice, testNs, sibling, label, nil, "tier", "keep")).To(Succeed())
		for _, n := range []string{matchA, matchB, sibling} {
			waitForFilePresent(repo, configMapRepoPath(testNs, n))
		}

		By("deleting only the matching subset as the actor")
		Expect(deleteConfigMapCollection(alice, testNs, "dctest="+label+",tier=doomed")).To(Succeed())

		By("asserting the matching files are removed and authored by the actor")
		waitForFileDeletedByActor(repo, configMapRepoPath(testNs, matchA))
		waitForFileDeletedByActor(repo, configMapRepoPath(testNs, matchB))

		By("asserting the non-matching sibling survives untouched")
		siblingPath := configMapRepoPath(testNs, sibling)
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			_, statErr := os.Stat(filepath.Join(repo.CheckoutDir, siblingPath))
			g.Expect(statErr).NotTo(HaveOccurred(), "the sibling must remain present in Git")
		}, 8*time.Second, 2*time.Second).Should(Succeed())
	})
})

// dcIntentBasePath is the GitTarget path this spec commits under; dcIntentActorAuthor
// is the Git author every removal in this spec must carry (the impersonated actor).
const (
	dcIntentBasePath    = "e2e/deletecollection-intent-test"
	dcIntentActorAuthor = "Alice Liddell <alice@configbutler.ai>"
)

// impersonatedConfigMapClient builds a clientset that impersonates asUser and carries
// OIDC display-name/email claims in user.extra, so writes it makes are attributed to
// "<displayName> <email>" in Git. system:masters keeps the impersonated requests
// authorized without per-user RBAC.
func impersonatedConfigMapClient(asUser, displayName, email string) (*kubernetes.Clientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if ctx := kubectlContext(); ctx != "" {
		overrides.CurrentContext = ctx
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	config.Impersonate = rest.ImpersonationConfig{
		UserName: asUser,
		Groups:   []string{"system:masters"},
		Extra: map[string][]string{
			"configbutler.ai/claims/display-name": {displayName},
			"configbutler.ai/claims/email":        {email},
		},
	}
	return kubernetes.NewForConfig(config)
}

// createLabeledConfigMap creates a ConfigMap carrying the per-spec dctest label (plus
// any extra key/value label pairs) and optional finalizers.
func createLabeledConfigMap(
	client *kubernetes.Clientset,
	ns, name, dctest string,
	finalizers []string,
	extraLabels ...string,
) error {
	labels := map[string]string{"dctest": dctest}
	for i := 0; i+1 < len(extraLabels); i += 2 {
		labels[extraLabels[i]] = extraLabels[i+1]
	}
	_, err := client.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Finalizers: finalizers},
		Data:       map[string]string{"test-key": "intent"},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create configmap %s: %w", name, err)
	}
	return nil
}

func deleteConfigMapCollection(client *kubernetes.Clientset, ns, labelSelector string) error {
	return client.CoreV1().ConfigMaps(ns).DeleteCollection(
		context.Background(),
		metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: labelSelector},
	)
}

func removeConfigMapFinalizers(client *kubernetes.Clientset, ns, name string) error {
	_, err := client.CoreV1().ConfigMaps(ns).Patch(
		context.Background(),
		name,
		k8stypes.MergePatchType,
		[]byte(`{"metadata":{"finalizers":null}}`),
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("clear finalizers on %s: %w", name, err)
	}
	return nil
}

// forceClearConfigMapFinalizers best-effort strips finalizers from every ConfigMap in
// the namespace so a Terminating object cannot block teardown.
func forceClearConfigMapFinalizers(client *kubernetes.Clientset, ns string) {
	list, err := client.CoreV1().ConfigMaps(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return
	}
	for i := range list.Items {
		if len(list.Items[i].Finalizers) > 0 {
			_ = removeConfigMapFinalizers(client, ns, list.Items[i].Name)
		}
	}
}

func configMapRepoPath(ns, name string) string {
	return path.Join(dcIntentBasePath, fmt.Sprintf("%s/configmaps/%s.yaml", ns, name))
}

func waitForFilePresent(repo *RepoArtifacts, repoPath string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		pullLatestRepoState(g, repo.CheckoutDir)
		_, err := os.Stat(filepath.Join(repo.CheckoutDir, repoPath))
		g.Expect(err).NotTo(HaveOccurred(), "file %s should be present in Git", repoPath)
	}, 2*time.Minute, 3*time.Second).Should(Succeed())
}

// waitForFileDeletedByActor asserts the file is gone from Git and the last commit that
// touched its path is authored by the impersonated actor (dcIntentActorAuthor).
func waitForFileDeletedByActor(repo *RepoArtifacts, repoPath string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		pullLatestRepoState(g, repo.CheckoutDir)
		_, statErr := os.Stat(filepath.Join(repo.CheckoutDir, repoPath))
		g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "file %s should be removed from Git", repoPath)
		author, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%an <%ae>", "--", repoPath)
		g.Expect(logErr).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(author)).To(Equal(dcIntentActorAuthor),
			"the removal commit must be authored by the delete requester")
	}, 3*time.Minute, 3*time.Second).Should(Succeed())
}

func commitHashForPath(repo *RepoArtifacts, repoPath string) string {
	out, err := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%H", "--", repoPath)
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}
