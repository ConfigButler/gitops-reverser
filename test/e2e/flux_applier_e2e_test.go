// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A FluxTrigger, named by a GitTarget's spec.applyTriggerRef, against a real Flux, from what a request is allowed to do to how GitOps
// Reverser learns that it has to make one. See docs/design/apply-trigger.md.
//
// The Kustomization applies its own folder (fluxApplierFolder) into a namespace the WatchRule does
// not watch, so nothing Flux applies is mirrored back and every commit on the branch is either this
// spec's API edit or this spec's Git-side push. Once the trigger holds it, the Kustomization is
// suspended, and no Flux Receiver is wired to the repository, so any apply inside the spec is one
// GitOps Reverser started.
var _ = Describe("Manager Flux Applier", Label("manager", "flux-applier"), Ordered, SpecPriority(90), func() {
	const (
		providerName = "flux-applier-provider"
		destName     = "flux-applier-dest"
		ruleName     = "flux-applier-rule"
		gitPath      = "apps/applier"
	)
	var (
		testNs, appliedNs        string
		repo                     *RepoArtifacts
		fluxName, fluxSecretName string
		receiverSecretName       string
		receiverHookID           int64
	)

	BeforeAll(func() {
		allowCrossNamespaceAppliers()
		testNs = testNamespaceFor("manager-flux-applier")
		appliedNs = testNs + "-applied"
		fluxName = "flux-applier-" + testNs
		fluxSecretName = fluxName + "-git"
		receiverSecretName = providerName + "-receiver"
		for _, ns := range []string{testNs, appliedNs} {
			_, _ = kubectlRun("create", "namespace", ns)
		}

		By("setting up the Gitea repo, its credentials and the GitProvider")
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-flux-applier-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)
		createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Succeeded", "")
		configureRepoOriginWithCredentials(repo, testNs)

		By("seeding the folder Flux applies, on main and on a release branch")
		pushAppliedColorFromOutside(repo, appliedNs, "blue")
		Expect(runGitIn(repo.CheckoutDir, "push", "origin", "HEAD:refs/heads/release")).To(Succeed())
	})

	AfterAll(func() {
		deleteRepoHook(repo.RepoName, receiverHookID)
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(destName, testNs)
		cleanupNamespacedResource(testNs, fluxTriggerResource, fluxName)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespacedResource(testNs, "secret", receiverSecretName)
		cleanupNamespacedResource("flux-system", "kustomization", fluxName)
		cleanupNamespacedResource("flux-system", "gitrepository", fluxName)
		cleanupNamespacedResource("flux-system", "secret", fluxSecretName)
		cleanupNamespace(appliedNs)
		cleanupNamespace(testNs)
	})

	// A trigger acts only while an eligible GitTarget references it. One nothing names, or one whose
	// Kustomization's source follows another branch than the GitTarget writes, holds nothing, and a
	// request for it never stamps that source; once the source is fixed, the trigger adopts the
	// Kustomization and Flux reaches the tip. Dropping the reference hands it back, and naming the
	// trigger again adopts it anew.
	It("holds the Kustomization only while an eligible GitTarget names its trigger", func() {
		By("creating the Kustomization, with a GitRepository that follows the release branch")
		applyFluxApplier(testNs, repo, fluxName, fluxSecretName, "release", "./"+fluxApplierFolder)

		By("creating a trigger no GitTarget names: it is Ready, and does nothing")
		createGitTarget(destName, testNs, providerName, gitPath, "main")
		createFluxTrigger(testNs, fluxName, fluxName, "")
		verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Ready", "True", "NotReferenced", "")
		verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Held", "False", "NotReferenced", "")
		Expect(kustomizationSuspended(Default, fluxName)).To(BeFalse(), "Flux applies it on its own")

		By("naming it from a GitTarget that writes main: the trigger is refused, and holds nothing")
		nameApplyTrigger(testNs, destName, "FluxTrigger", fluxName, "")
		verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Configured", "False", "SourceMismatch",
			`applies branch "release", not main`)
		verifyResourceCondition("gittarget", destName, testNs, "ApplyTriggerReady", "False", "SourceMismatch",
			`applies branch "release", not main`)
		Expect(kustomizationSuspended(Default, fluxName)).To(BeFalse(), "a trigger that is not bound adopts nothing")
		Expect(fluxTriggerEvents(Default, testNs, fluxName, "Adopted")).To(BeEmpty())
		Expect(fluxSourceStamp(fluxName)).To(BeEmpty(), "nothing has asked Flux to fetch yet")

		By("mirroring a first API edit, so the branch has a tip GitOps Reverser pushed")
		Expect(applyFromTemplate("test/e2e/templates/manager/watchrule-resources.tmpl", struct {
			Name, Namespace, DestinationName, Resources string
		}{Name: ruleName, Namespace: testNs, DestinationName: destName, Resources: `"configmaps"`},
			testNs)).To(Succeed(), "failed to apply the WatchRule")
		applyConfigMap(testNs, "applier-first-edit")
		Eventually(func(g Gomega) {
			g.Expect(runGitIn(repo.CheckoutDir, "fetch", "origin", "main")).To(Succeed())
			g.Expect(gitShowFiles(repo.CheckoutDir)).To(ContainSubstring("applier-first-edit"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		// GitOps Reverser finds the Git-side push the hard way: its next push is rejected, and the
		// fetch that follows is a ForeignCommit, which asks every applier on the branch.
		By("pushing a Git-side change, then an API edit that finds it")
		pushAppliedColorFromOutside(repo, appliedNs, "green")
		applyConfigMap(testNs, "applier-second-edit")

		By("the ForeignCommit is refused at the patch, and the source on release is never stamped")
		Eventually(func(g Gomega) {
			failed := gitTargetEventMessages(g, testNs, destName, "ApplyRequestFailed")
			g.Expect(failed).To(ContainSubstring("(ForeignCommit)"))
			g.Expect(failed).To(ContainSubstring(`fetches branch "release", not main`))
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(fluxSourceStamp(fluxName)).To(BeEmpty(),
				"a GitRepository that follows another branch must never be stamped for this one")
		}, 20*time.Second, 2*time.Second).Should(Succeed())

		// Changing the GitRepository's branch binds the trigger, which adopts the Kustomization. The
		// branch worker keeps the ForeignCommit across the rejected attempts and decides it again
		// after its backoff; Flux may also apply the new artifact on its own before the adoption.
		By("pointing the GitRepository at main: the trigger adopts the Kustomization, and Flux reaches the tip")
		head := remoteHeadSHA(Default, repo.CheckoutDir)
		_, err := kubectlRunInNamespace("flux-system", "patch", "gitrepository", fluxName, "--type=merge", "-p",
			`{"spec":{"ref":{"branch":"main"}}}`)
		Expect(err).NotTo(HaveOccurred(), "failed to point the GitRepository at main")
		requestTriggerReconcile(testNs, fluxTriggerResource, fluxName)
		verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Ready", "True", "Succeeded", "")
		verifyResourceCondition("gittarget", destName, testNs, "ApplyTriggerReady", "True", "Succeeded", "")
		Expect(fluxTriggerEvents(Default, testNs, fluxName, "Adopted")).
			To(ContainSubstring("suspended Kustomization flux-system/" + fluxName))
		Eventually(func(g Gomega) {
			g.Expect(fluxLastAppliedRevision(g, fluxName)).To(ContainSubstring(head))
			g.Expect(appliedColor(g, appliedNs)).To(Equal("green"))
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
		verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Held", "True", "Suspended", "")
		Expect(kustomizationSuspended(Default, fluxName)).To(BeTrue(), "the trigger holds its Kustomization")
		others, err := kubectlRunInNamespace(testNs, "get", fluxTriggerResource, fluxName,
			"-o", "jsonpath={.status.source.otherConsumers}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(others)).To(BeEmpty(), "its Kustomization is the GitRepository's only consumer")

		By("dropping the reference: the trigger hands the Kustomization back, and is idle again")
		_, err = kubectlRunInNamespace(testNs, "patch", "gittarget", destName, "--type=merge", "-p",
			`{"spec":{"applyTriggerRef":null}}`)
		Expect(err).NotTo(HaveOccurred(), "failed to drop the GitTarget's applyTriggerRef")
		verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Held", "False", "NotReferenced", "")
		verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Ready", "True", "NotReferenced", "")
		Eventually(func(g Gomega) {
			g.Expect(kustomizationSuspended(g, fluxName)).To(BeFalse())
			g.Expect(kustomizationManagers(g, fluxName)).NotTo(ContainSubstring("gitops-reverser"))
			g.Expect(fluxTriggerEvents(g, testNs, fluxName, "Released")).
				To(ContainSubstring("handed Kustomization flux-system/" + fluxName + " back to Flux"))
		}, time.Minute, 2*time.Second).Should(Succeed())

		By("naming the trigger again: it adopts the Kustomization anew")
		nameApplyTrigger(testNs, destName, "FluxTrigger", fluxName, "")
		verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Held", "True", "Suspended", "")
		Expect(kustomizationSuspended(Default, fluxName)).To(BeTrue())
	})

	// With no receiver and no API edit, the only thing that can find a Git-side push is the
	// refresher on the GitTarget's steady tick. This is the guarantee that a missing webhook costs
	// latency and never an apply (docs/design/apply-trigger.md).
	// The e2e install refreshes after 90s, so the wait is the control plane's reconcile cadence of
	// about five minutes, as in the refresh-remote spec.
	It("applies a Git-side push the refresher finds, with no receiver and no API edit", func() {
		Eventually(func(g Gomega) {
			g.Expect(appliedColor(g, appliedNs)).To(Equal("green"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed(), "the previous spec left Flux at the tip")
		foreignBefore := strings.Count(
			gitTargetEventMessages(Default, testNs, destName, "ApplyRequested"), "(ForeignCommit)")

		By("pushing a Git-side change, and nothing else")
		head := pushAppliedColorFromOutside(repo, appliedNs, "orange")

		By("the steady tick's refresh finds it, and Flux applies it")
		Eventually(func(g Gomega) {
			g.Expect(fluxLastAppliedRevision(g, fluxName)).To(ContainSubstring(head))
			g.Expect(appliedColor(g, appliedNs)).To(Equal("orange"))
		}, 7*time.Minute, 5*time.Second).Should(Succeed())
		Expect(strings.Count(gitTargetEventMessages(Default, testNs, destName, "ApplyRequested"),
			"(ForeignCommit)")).To(BeNumerically(">", foreignBefore),
			"the apply must come from a ForeignCommit request, not from Flux's own schedule")
	})

	// The receiver's handler and the worker's discovery of a foreign push were each unit-tested, and
	// nothing proved them wired together in a deployed operator. Here Gitea itself delivers the push
	// event, signed, to the operator's receiver Service, and nothing else happens on the branch: no
	// API edit surfaces the push, and the refresher's tick is minutes away.
	It("applies a Git-side push the Git host reports, with nothing else to surface it", func() {
		By("configuring the GitProvider's receiver, and registering it as a Gitea webhook")
		receiverHookID = registerOperatorReceiver(testNs, providerName, receiverSecretName, repo.RepoName)

		Eventually(func(g Gomega) {
			g.Expect(appliedColor(g, appliedNs)).To(Equal("orange"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed(), "the previous spec left Flux at the tip")
		foreignBefore := strings.Count(
			gitTargetEventMessages(Default, testNs, destName, "ApplyRequested"), "(ForeignCommit)")

		By("pushing a Git-side change, and nothing else")
		head := pushAppliedColorFromOutside(repo, appliedNs, "purple")

		By("Flux applies it, because GitOps Reverser was told and asked")
		Eventually(func(g Gomega) {
			g.Expect(fluxLastAppliedRevision(g, fluxName)).To(ContainSubstring(head))
			g.Expect(appliedColor(g, appliedNs)).To(Equal("purple"))
		}, 90*time.Second, 2*time.Second).Should(Succeed())
		Expect(strings.Count(gitTargetEventMessages(Default, testNs, destName, "ApplyRequested"),
			"(ForeignCommit)")).To(BeNumerically(">", foreignBefore),
			"the apply must come from a ForeignCommit request, not from Flux's own schedule")
	})

	// The suspended lifecycle's two promises beyond an ordinary apply: a failed apply is suspended
	// again, so Flux's own retryInterval retry cannot fire and the worker owns the retry; and a
	// Kustomization resumed by hand (`flux resume`) is suspended again, with an Event.
	It("holds the Kustomization suspended through a failed apply and a resume by hand", func() {
		laterNs := appliedNs + "-later"
		DeferCleanup(cleanupNamespace, laterNs)

		By("pushing a commit whose apply fails: a ConfigMap into a namespace that does not exist yet")
		pushFileFromOutside(repo, fluxApplierFolder+"/later.yaml", fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: flux-applied-later
  namespace: %s
data:
  color: teal
`, laterNs), "e2e: another writer adds a ConfigMap in a namespace that does not exist")

		By("the operation fails, and the Kustomization is suspended again after it")
		var failed triggerOperation
		Eventually(func(g Gomega) {
			failed = readTriggerOperation(g, testNs, fluxTriggerResource, fluxName)
			g.Expect(failed.Phase).To(Equal("Failed"))
			g.Expect(kustomizationSuspended(g, fluxName)).To(BeTrue())
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("once the namespace exists, the worker's own retry applies it")
		_, err := kubectlRun("create", "namespace", laterNs)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			_, err := kubectlRunInNamespace(laterNs, "get", "configmap", "flux-applied-later")
			g.Expect(err).NotTo(HaveOccurred())
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			done := readTriggerOperation(g, testNs, fluxTriggerResource, fluxName)
			g.Expect(done.Phase).To(Equal("Succeeded"))
			g.Expect(done.Token).NotTo(Equal(failed.Token),
				"applied by an operation the worker decided, not by Flux's retryInterval retry")
			g.Expect(kustomizationSuspended(g, fluxName)).To(BeTrue())
		}, time.Minute, 2*time.Second).Should(Succeed())

		By("resuming the Kustomization by hand, as `flux resume` does")
		_, err = kubectlRunInNamespace("flux-system", "patch", "kustomization", fluxName, "--type=merge",
			"--field-manager=flux", "-p", `{"spec":{"suspend":false}}`)
		Expect(err).NotTo(HaveOccurred())
		requestTriggerReconcile(testNs, fluxTriggerResource, fluxName)
		Eventually(func(g Gomega) {
			g.Expect(fluxTriggerEvents(g, testNs, fluxName, "ResumedElsewhere")).
				To(ContainSubstring("was resumed outside GitOps Reverser"))
			g.Expect(kustomizationSuspended(g, fluxName)).To(BeTrue(), "suspended again")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
		verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Held", "True", "Suspended", "")
	})

	// Deleting the trigger hands the Kustomization back: its finalizer waits until nothing is
	// running and releases spec.suspend, so Flux applies on its own again.
	It("hands the Kustomization back to Flux when the trigger is deleted", func() {
		_, err := kubectlRunInNamespace(testNs, "delete", fluxTriggerResource, fluxName, "--timeout=2m")
		Expect(err).NotTo(HaveOccurred(), "the finalizer must let the trigger go")
		Eventually(func(g Gomega) {
			g.Expect(kustomizationSuspended(g, fluxName)).To(BeFalse())
			g.Expect(kustomizationManagers(g, fluxName)).NotTo(ContainSubstring("gitops-reverser"))
		}, time.Minute, 2*time.Second).Should(Succeed())
	})
})

// fluxApplierFolder is the folder the Manager Flux Applier spec's Kustomization applies.
const fluxApplierFolder = "flux/applier"

// applyFluxApplier creates a GitRepository following branch and a Kustomization applying path from
// it, both on a 24h interval, and waits for the Kustomization's first apply, made before any trigger
// holds it. The Flux specs share it.
func applyFluxApplier(testNs string, repo *RepoArtifacts, name, secretName, branch, path string) {
	GinkgoHelper()
	// The in-cluster Gitea test credential, read from the cluster at runtime, base64-encoded as the
	// template's Secret expects.
	fields := struct {
		Namespace, SecretName, Name, RepoURL, Branch, Interval, Username, Password string
	}{
		Namespace: "flux-system", SecretName: secretName, Name: name, RepoURL: repo.RepoURLHTTP,
		Branch: branch, Interval: "24h",
	}
	checkout := gitCheckout{secretNamespace: testNs, gitSecretName: repo.GitSecretHTTP}
	fields.Username, fields.Password = checkout.readGitCredentialSecretDataBase64()
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/flux-gitrepository-http.tmpl", fields,
		"flux-system")).To(Succeed(), "failed to apply the Flux GitRepository")
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/flux-kustomization.tmpl", struct {
		Namespace, Name, Path, SourceName, Interval, DependsOn string
		Prune, Wait                                            bool
		TargetNS                                               string
		HasTargetNS                                            bool
		DecryptionProvider, DecryptionSecretName               string
	}{
		Namespace: "flux-system", Name: name, Path: path, SourceName: name, Interval: "24h",
	}, "flux-system")).To(Succeed(), "failed to apply the Flux Kustomization")
	_, err := kubectlRunInNamespace("flux-system", "wait", "--for=condition=Ready",
		"kustomization/"+name, "--timeout=3m")
	Expect(err).NotTo(HaveOccurred(), "the Kustomization must apply its folder once")
}

// pushAppliedColorFromOutside writes the ConfigMap the Kustomization applies, with the given
// color, and pushes it to main the way another writer would (pushFileFromOutside).
func pushAppliedColorFromOutside(repo *RepoArtifacts, appliedNs, color string) string {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: flux-applied
  namespace: %s
data:
  color: %s
`, appliedNs, color)
	return pushFileFromOutside(repo, fluxApplierFolder+"/configmap.yaml", manifest,
		"e2e: another writer sets the applied color to "+color)
}

// pushFileFromOutside writes one file and pushes it to main the way another writer would, leaving
// it checked out, and returns the commit it pushed. It retries, because GitOps Reverser may move
// the tip at any moment and losing that race says nothing about the behavior under test.
func pushFileFromOutside(repo *RepoArtifacts, path, content, message string) string {
	GinkgoHelper()
	var head string
	Eventually(func() error {
		// The first push seeds an empty repository, which has no main to build on.
		start := [][]string{{"checkout", "--orphan", "main"}, {"rm", "-rf", "--ignore-unmatch", "."}}
		if _, err := gitRun(repo.CheckoutDir, "fetch", "origin", "main"); err == nil {
			start = [][]string{{"checkout", "-B", "main", "origin/main"}, {"reset", "--hard", "origin/main"}}
		}
		for _, args := range start {
			if out, err := gitRun(repo.CheckoutDir, args...); err != nil {
				return fmt.Errorf("git %v: %w: %s", args, err, out)
			}
		}
		dest := filepath.Join(repo.CheckoutDir, path)
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(dest, []byte(content), 0o600); err != nil {
			return err
		}
		for _, args := range [][]string{
			{"add", path},
			{"commit", "-m", message},
			{"push", "origin", "HEAD:main"},
		} {
			if out, err := gitRun(repo.CheckoutDir, args...); err != nil {
				return fmt.Errorf("git %v: %w: %s", args, err, out)
			}
		}
		out, err := gitRun(repo.CheckoutDir, "rev-parse", "HEAD")
		head = strings.TrimSpace(out)
		return err
	}, seedPushTimeout, seedPushInterval).Should(Succeed(), "the outside writer kept losing the push race")
	return head
}

// appliedColor is the color of the ConfigMap the Kustomization applies.
func appliedColor(g Gomega, appliedNs string) string {
	out, err := kubectlRunInNamespace(appliedNs, "get", "configmap", "flux-applied", "-o", "jsonpath={.data.color}")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// fluxLastAppliedRevision is the revision the Kustomization last applied.
func fluxLastAppliedRevision(g Gomega, name string) string {
	out, err := kubectlRunInNamespace("flux-system", "get", "kustomization", name,
		"-o", "jsonpath={.status.lastAppliedRevision}")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// gitTargetEventMessages is every message of the Events with reason recorded on a GitTarget.
func gitTargetEventMessages(g Gomega, testNs, gitTarget, reason string) string {
	out, err := kubectlRunInNamespace(testNs, "get", "events", "--field-selector",
		"involvedObject.kind=GitTarget,involvedObject.name="+gitTarget+",reason="+reason,
		"-o", "jsonpath={.items[*].message}")
	g.Expect(err).NotTo(HaveOccurred())
	return out
}

// registerOperatorReceiver gives a GitProvider a gitea receiver whose token is a new Secret, and
// registers the path it serves as a push webhook on the Gitea repository, pointed at the operator's
// receiver Service. It returns the hook's id, for deleteRepoHook.
func registerOperatorReceiver(testNs, providerName, secretName, repoName string) int64 {
	GinkgoHelper()
	token := randomToken()
	_, err := kubectlRunInNamespace(testNs, "create", "secret", "generic", secretName, "--from-literal=token="+token)
	Expect(err).NotTo(HaveOccurred(), "failed to create the receiver Secret")
	_, err = kubectlRunInNamespace(testNs, "patch", "gitprovider", providerName, "--type=merge", "-p",
		fmt.Sprintf(`{"spec":{"receiver":{"type":"gitea","secretRef":{"name":%q}}}}`, secretName))
	Expect(err).NotTo(HaveOccurred(), "failed to set spec.receiver")
	var webhookPath string
	Eventually(func(g Gomega) {
		out, err := kubectlRunInNamespace(testNs, "get", "gitprovider", providerName,
			"-o", "jsonpath={.status.webhookPath}")
		g.Expect(err).NotTo(HaveOccurred())
		webhookPath = strings.TrimSpace(out)
		g.Expect(webhookPath).NotTo(BeEmpty())
	}, time.Minute, 2*time.Second).Should(Succeed())
	gitea := giteaTestInstance()
	ctx, cancel := gitea.Context()
	defer cancel()
	hook, err := gitea.Client().CreateGiteaWebhook(ctx, gitea.Org, repoName,
		fmt.Sprintf("http://gitops-reverser-receiver.%s.svc.cluster.local%s", resolveE2ENamespace(), webhookPath),
		token, []string{"push"})
	Expect(err).NotTo(HaveOccurred(), "failed to register the receiver as a Gitea webhook")
	return hook.ID
}

// deleteRepoHook deletes a Gitea webhook registerOperatorReceiver made; zero is none.
func deleteRepoHook(repoName string, hookID int64) {
	if hookID == 0 {
		return
	}
	gitea := giteaTestInstance()
	ctx, cancel := gitea.Context()
	defer cancel()
	_ = gitea.Client().DeleteRepoHook(ctx, gitea.Org, repoName, hookID)
}

// randomToken is a receiver secret for one spec.
func randomToken() string {
	GinkgoHelper()
	buf := make([]byte, 24)
	_, err := rand.Read(buf)
	Expect(err).NotTo(HaveOccurred())
	return hex.EncodeToString(buf)
}
