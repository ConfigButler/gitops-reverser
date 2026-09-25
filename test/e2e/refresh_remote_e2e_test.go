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

// The refresher, from the outside: a healthy, converged, IDLE GitTarget notices that its branch
// moved, with nobody asking it to.
//
// This is the spec the flag it replaces could never have had. Expiring base trust could only make
// an idle target act by scheduling a full re-check, so what an operator would have observed was a
// cluster snapshot; what is observed here is one ref advertisement turning into status an
// operator can read, and no commit at all.
//
// The two assertions that make it a boundary rather than a rule are the last two: the branch must
// hold exactly the commit the OTHER writer made, and the folder's republished shape must be the
// one that writer left behind.
//
// Timing. The refresh rides the GitTarget reconcile, and a converged target requeues on the
// 5-minute steady interval, so the bound here is one steady tick plus the refresh itself — not
// the 30s --git-refresh-interval config/deployment.yaml sets, which only decides how old an
// observation may be when that tick arrives.
//
// status.remote is SAMPLED onto that same tick: proving where the branch is and publishing it are
// two different rates, and nothing in the data plane wakes a target because a branch moved. So
// every wait on that stanza below is one steady tick wide, including the one after our OWN push.
// That is the contract, not slack: see docs/spec/status-conditions-guide.md.
//
// No reconcile-request annotation is used anywhere in this spec: an operator asking by hand is
// precisely the case this feature exists to remove.
var _ = Describe("Manager Remote Refresh", Label("manager", "refresh"), Ordered, func() {
	var (
		testNs       string
		repo         *RepoArtifacts
		providerName = "refresh-provider"
		destName     = "refresh-dest"
		ruleName     = "refresh-rule"
		gitPath      = "apps/refresh"
	)

	BeforeAll(func() {
		By("creating the test namespace")
		testNs = testNamespaceFor("manager-refresh")
		_, _ = kubectlRun("create", "namespace", testNs)

		By("setting up Gitea repo and credentials")
		repo = SetupRepo(resolveE2EContext(), testNs,
			fmt.Sprintf("e2e-refresh-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("creating the GitProvider")
		createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Succeeded", "")
	})

	AfterAll(func() {
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespace(testNs)
	})

	It("follows a branch somebody else moved, and writes nothing", func() {
		By("creating a GitTarget that mirrors ConfigMaps, and letting it publish once")
		createGitTarget(destName, testNs, providerName, gitPath, "main")
		Expect(applyFromTemplate("test/e2e/templates/manager/watchrule-resources.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
			Resources       string
		}{Name: ruleName, Namespace: testNs, DestinationName: destName, Resources: `"configmaps"`},
			testNs)).To(Succeed(), "failed to apply the WatchRule")

		Expect(applyFromTemplate("test/e2e/templates/manager/configmap.tmpl", struct {
			Name      string
			Namespace string
		}{Name: "refresh-seed", Namespace: testNs},
			testNs)).To(Succeed(), "failed to apply the seed ConfigMap")

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			g.Expect(gitShowFiles(repo.CheckoutDir)).To(ContainSubstring("refresh-seed"),
				"the target has to publish once before it can be idle")
		}, 150*time.Second, 2*time.Second).Should(Succeed())

		// The push it just made IS an observation of the remote, made on the connection the push
		// was opening anyway. That is the whole of decision 2, and it is visible here: status
		// names the revision the server accepted, and says a Push proved it.
		By("status.remote names the revision our own push established")
		var pushedRevision string
		Eventually(func(g Gomega) {
			pushedRevision = gitTargetRemoteField(g, destName, testNs, "revision")
			g.Expect(pushedRevision).NotTo(BeEmpty(), "a target that has pushed knows where its branch is")
			g.Expect(gitTargetRemoteField(g, destName, testNs, "verifiedBy")).To(Equal("Push"))
			g.Expect(pushedRevision).To(Equal(remoteHeadSHA(g, repo.CheckoutDir)))
		}, 7*time.Minute, 5*time.Second).Should(Succeed())

		verifiedBefore := gitTargetRemoteField(Default, destName, testNs, "lastVerifiedAt")
		Expect(verifiedBefore).NotTo(BeEmpty(), "the observation is dated, or nothing can report its own staleness")
		commitsBefore := remoteCommitCount(repo.CheckoutDir)

		// Somebody else moves the branch, and changes the folder's SHAPE while they are at it, so
		// the placement projection has something to follow too.
		By("another writer pushes straight to Gitea, turning the folder into a kustomize root")
		configureRepoOriginWithCredentials(repo, testNs)
		pushKustomizationFromOutside(repo, gitPath)
		movedRevision := remoteHeadSHA(Default, repo.CheckoutDir)
		Expect(movedRevision).NotTo(Equal(pushedRevision))

		// Nobody annotates anything. The steady tick arrives, the worker spends one ref
		// advertisement, and what it saw reaches status.
		By("the idle target notices, with nobody asking it to")
		Eventually(func(g Gomega) {
			g.Expect(gitTargetRemoteField(g, destName, testNs, "revision")).To(Equal(movedRevision),
				"an idle target must follow the branch somebody else moved")
			g.Expect(gitTargetRemoteField(g, destName, testNs, "verifiedBy")).To(Equal("Fetch"),
				"we went and looked, and that is how a foreign push is read off kubectl")
			g.Expect(gitTargetRemoteField(g, destName, testNs, "lastVerifiedAt")).NotTo(Equal(verifiedBefore),
				"the clock has to move with the fact it dates")
		}, 7*time.Minute, 5*time.Second).Should(Succeed())

		// This is the assertion that makes §3.6's boundary a test rather than a rule.
		By("and it writes nothing at all")
		Expect(remoteCommitCount(repo.CheckoutDir)).To(Equal(commitsBefore+1),
			"the only new commit on the branch must be the one the other writer made")

		By("while the folder's republished shape follows the writer who changed it")
		Eventually(func(g Gomega) {
			g.Expect(gitTargetField(g, destName, testNs, "{.status.placement.mode}")).
				To(Equal("KustomizeRoot"),
					"re-reading the folder is free, and it is most of what an operator reads")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("and the GitProvider says when its credential was last proved")
		Eventually(func(g Gomega) {
			out, err := kubectlRunInNamespace(testNs, "get", "gitprovider", providerName,
				"-o", "jsonpath={.status.lastVerifiedAt}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty(),
				"a provider whose check succeeds must date it, so a later failure reads as a duration")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})
})

// gitTargetRemoteField reads one field of status.remote.
func gitTargetRemoteField(g Gomega, name, namespace, field string) string {
	return gitTargetField(g, name, namespace, fmt.Sprintf("{.status.remote.%s}", field))
}

func gitTargetField(g Gomega, name, namespace, jsonPath string) string {
	out, err := kubectlRunInNamespace(namespace, "get", "gittarget", name, "-o", "jsonpath="+jsonPath)
	g.Expect(err).NotTo(HaveOccurred(), "failed to read %s of GitTarget %s", jsonPath, name)
	return strings.TrimSpace(out)
}

// remoteHeadSHA is where the branch is, read through the seeded checkout.
func remoteHeadSHA(g Gomega, checkoutDir string) string {
	return remoteHead(g, checkoutDir)
}

// pushKustomizationFromOutside adds a kustomization.yaml to the target's folder and pushes it
// directly, the way a colleague or another tool would. It retries, because the controller may own
// the tip at any moment and losing that race says nothing about the behaviour under test.
func pushKustomizationFromOutside(repo *RepoArtifacts, gitPath string) {
	GinkgoHelper()

	Eventually(func() error {
		if _, err := gitRun(repo.CheckoutDir, "fetch", "origin", "main"); err != nil {
			return err
		}
		if out, err := gitRun(repo.CheckoutDir, "checkout", "-B", "main", "origin/main"); err != nil {
			return fmt.Errorf("checkout: %w: %s", err, out)
		}
		if out, err := gitRun(repo.CheckoutDir, "reset", "--hard", "origin/main"); err != nil {
			return fmt.Errorf("reset: %w: %s", err, out)
		}
		dest := filepath.Join(repo.CheckoutDir, gitPath)
		if err := os.MkdirAll(dest, 0o750); err != nil {
			return err
		}
		body := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"
		if err := os.WriteFile(filepath.Join(dest, "kustomization.yaml"), []byte(body), 0o600); err != nil {
			return err
		}
		if out, err := gitRun(repo.CheckoutDir, "add", gitPath); err != nil {
			return fmt.Errorf("add: %w: %s", err, out)
		}
		if out, err := gitRun(repo.CheckoutDir, "commit", "-m",
			"e2e: another writer changes the folder"); err != nil {
			return fmt.Errorf("commit: %w: %s", err, out)
		}
		if out, err := gitRun(repo.CheckoutDir, "push", "origin", "HEAD:main"); err != nil {
			return fmt.Errorf("push: %w: %s", err, out)
		}
		return nil
	}, seedPushTimeout, seedPushInterval).Should(Succeed(),
		"the outside writer kept losing the push race with the controller")
}
