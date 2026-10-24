// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Argo CD half of the bi-directional corner (docs/spec/e2e-bi-directional-corner.md).
//
// Argo CD is only installed for this corner, by the `_argocd-installed` Taskfile
// node. `task test-e2e` skips these specs; `task test-e2e-bi-directional` runs them.
//
// Every spec runs the standard (docs/bi-directional-argocd.md): the ArgoCDClaim the
// GitTarget names holds the Application's automated sync off, so every sync it runs is
// one GitOps Reverser started, pinned to a commit it checked. Argo CD's own Gitea
// webhook stays wired, and is shown to be harmless: with automated sync off it only
// refreshes. Two specs take the switch: one switches automated sync on and proves the
// applier claim switches it off again, and one generates the Application from an
// ApplicationSet, whose switch the applier claim never writes.
//
// Everything is driven through kubectl, so the devcontainer needs no `argocd`
// binary.
const (
	// Argo CD stamps this onto every non-CRD object it applies. Its default
	// resource-tracking method is `annotation` (NOT `label`), so this annotation —
	// and not `app.kubernetes.io/instance` — is what gitops-reverser observes on
	// the live object. internal/sanitize must strip it before writing to Git.
	argoTrackingIDAnnotation = "argocd.argoproj.io/tracking-id"
	// Written by Argo's default client-side apply. Already covered by the existing
	// `kubectl.kubernetes.io/` prefix strip; asserted here against a real Argo.
	kubectlLastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

	// Shared secret for the Gitea -> Argo CD webhook. Gitea sends a Gogs-style
	// X-Gogs-Signature (Gitea emits X-Gogs-* headers next to its native X-Gitea-*
	// ones), which argocd-server validates against `webhook.gogs.secret` in
	// argocd-secret; the Gitea hook is created with the same value.
	argoWebhookSecret = "e2e-argocd-webhook-secret"

	// The initiator GitOps Reverser's syncs carry (applier.ArgoCDInitiator).
	argoReverserInitiator = "gitops-reverser"

	argoSyncTimeout = 120 * time.Second
	// A Git-side push reaches GitOps Reverser's receiver, which decides a
	// ForeignCommit after a flush, and Argo CD then syncs: seconds, far below the
	// 10m refresher, so this can only pass through the receiver.
	argoRequestedSyncTimeout = 90 * time.Second
)

// argoBiDirectionalRepo holds the file-local repo fixtures for the main describe block.
var argoBiDirectionalRepo *RepoArtifacts

type argoBiDirectionalRun struct {
	gitCheckout

	testID   string
	testNs   string
	argoNs   string
	repoName string
	repoURL  string

	appName        string
	repoSecretName string

	gitProviderName    string
	gitTargetName      string
	watchRuleName      string
	receiverSecretName string
	livePath           string

	// One order per spec, so the specs never contend for the same object.
	syncedOrderName    string
	editedOrderName    string
	automatedOrderName string

	// The ApplicationSet spec's own folder, GitTarget, applier claim and generated Application.
	appSetPath       string
	appSetName       string
	appSetTargetName string
	generatedAppName string
	appSetClaimName  string
}

// argoAppConfig is the knob set the specs turn. The Application is re-applied (not
// patched) each time, so dropping a field really removes it.
type argoAppConfig struct {
	Name                 string
	ArgoNamespace        string
	RepoURL              string
	Branch               string
	Path                 string
	DestinationNamespace string

	// Kustomize renders the path as a kustomization instead of a recursive directory.
	Kustomize bool
	// Automated turns automated sync ON, which the applier claim switches off again.
	Automated bool
}

var _ = Describe("Bi Directional (Argo CD)", Label("bi-directional", "argocd"), Ordered, func() {
	var run argoBiDirectionalRun
	var testNs string
	var receiverHookID int64

	BeforeAll(func() {
		skipUnlessBiDirectionalEnabled()
		requireArgoCDInstalled()
		grantAppliersInAnyNamespace()

		By("creating test namespace")
		testNs = testNamespaceFor("argo-bi-directional")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up Gitea repo and credentials for the Argo CD bi-directional test")
		argoBiDirectionalRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-argo-bi-directional-%d", GinkgoRandomSeed()),
		)

		By("applying git secrets to test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", argoBiDirectionalRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		run = newArgoBiDirectionalRun(testNs, argoBiDirectionalRepo, "argo-bi")
		run.assertCheckoutReady()

		By("installing the IceCreamOrder CRD directly")
		// Unlike the Flux spec, the CRD does not travel through Git here: Argo's
		// role in this spec is to apply the *live path*, and a CRD-then-CR ordering
		// test would only re-cover what the Flux spec already covers.
		Expect(applyIceCreamCRD(crdGroupArgoBiDirectional)).To(Succeed())
		run.waitForCRDEstablished()

		By("registering the Gitea repository with Argo CD")
		run.applyArgoRepoSecret()

		By("wiring a Gitea -> Argo CD webhook, which with automated sync off only refreshes")
		run.configureArgoWebhookSecret()
		run.ensureArgoWebhook()
	})

	AfterAll(func() {
		if !biDirectionalEnabled() {
			return
		}
		// Delete the Application first so Argo stops reconciling while the
		// namespace goes away. Without a resources-finalizer it does not prune,
		// so the IceCreamOrders die with the namespace.
		cleanupNamespacedResource(run.argoNs, "application.argoproj.io", run.appName)
		cleanupNamespacedResource(run.argoNs, "secret", run.repoSecretName)
		deleteRepoHook(run.repoName, receiverHookID)
		cleanupWatchRule(run.watchRuleName, testNs)
		cleanupGitTarget(run.gitTargetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", run.gitProviderName)
		cleanupNamespacedResource(testNs, "gitreceiver", run.receiverSecretName)
		cleanupNamespacedResource(testNs, "secret", run.receiverSecretName)
		cleanupClusterResource("crd", iceCreamCRDName(crdGroupArgoBiDirectional))
		cleanupNamespace(testNs)
	})

	// A Git-side push reaches the cluster only through GitOps Reverser: its receiver
	// hears of the push, the worker decides a ForeignCommit after a flush, and the
	// sync it starts is pinned to that commit. Nothing GitOps Reverser pushes itself
	// is synced; naming the Application converges it once. And what Argo CD stamps on
	// the objects it applies stays out of Git.
	It("syncs a Git-side push through a pinned ForeignCommit, and keeps Argo CD's bookkeeping out of Git", func() {
		// SetupRepo leaves the Gitea repo EMPTY, so seed the branch first: the
		// GitTarget needs a main to bootstrap. Argo CD has nothing to sync until the
		// Git-side push below, and with automated sync off it would not sync it anyway.
		By("seeding the branch, and creating the Application with automated sync off")
		Expect(run.commitAllAndPush("argo bi-directional: seed main")).To(Succeed())
		run.applyArgoApp(run.baseAppConfig())

		By("enabling gitops-reverser on the shared live path, naming the Application as the applier")
		receiverHookID = run.startReverserPipeline()

		By("naming the Application converges it once, after the folder's initial snapshot is in Git")
		Eventually(func(g Gomega) {
			operation := run.claimOperation(g)
			g.Expect(operation.Reason).To(Equal("CatchUp"))
			g.Expect(operation.Phase).To(Equal("Succeeded"))
		}, argoRequestedSyncTimeout, biPollInterval).Should(Succeed())
		converged := run.claimOperation(Default)

		// Absorbs the reverser's bootstrap commit, so the counts below measure the
		// Git-side push and Argo CD's sync, and nothing else.
		baselineCommitCount := run.waitForStableRemoteCommitCount(biStableCountShortWait)
		Expect(run.claimOperation(Default)).To(Equal(converged),
			"GitOps Reverser's own pushes ask Argo CD for nothing more")

		By("pushing a clean IceCreamOrder from the Git side, and nothing else")
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.syncedOrderName,
			Namespace:    testNs,
			CustomerName: "Alice",
			Container:    "Cone",
			Scoops:       []iceCreamScoop{{Flavor: "Vanilla", Quantity: 2}},
			Toppings:     []string{"Sprinkles"},
		})
		Expect(run.commitAllAndPush("argo bi-directional: add icecream order")).To(Succeed())
		head := run.gitHEAD()

		By("GitOps Reverser asks Argo CD to sync exactly that commit, as a ForeignCommit")
		run.waitForReverserSync(head, "ForeignCommit")
		run.waitForOrderContainer(run.syncedOrderName, "Cone")
		// The branch worker writes the operation on the applier claim at every transition, so nothing
		// has to reconcile for it to show.
		Eventually(func(g Gomega) {
			operation := run.claimOperation(g)
			g.Expect(operation.Reason).To(Equal("ForeignCommit"))
			g.Expect(operation.Commit).To(Equal(head))
			g.Expect(operation.Phase).To(Equal("Succeeded"))
		}, biEventuallyTimeout, biPollInterval).Should(Succeed())

		By("verifying Argo CD stamped its bookkeeping onto the live object")
		liveAnnotations := run.liveOrderAnnotations(run.syncedOrderName)
		Expect(liveAnnotations).To(HaveKeyWithValue(
			argoTrackingIDAnnotation, run.expectedTrackingID(run.syncedOrderName)),
			"Argo CD's default resource-tracking method should stamp the tracking-id annotation")
		Expect(liveAnnotations).To(HaveKey(kubectlLastAppliedAnnotation),
			"Argo CD's default apply is client-side, which writes last-applied-configuration")

		By("verifying none of that bookkeeping is committed, and the sync made no commit at all")
		// If any of it leaked, the sanitized live object would differ from the file
		// already in Git and the reverser would commit — so the count would grow.
		run.consistentlyExpectRemoteCommitCount(baselineCommitCount+1, biStableCountLongWait)
		committed := run.readCommittedOrder(run.syncedOrderName)
		Expect(committed).NotTo(ContainSubstring(argoTrackingIDAnnotation),
			"Argo CD's tracking-id is controller bookkeeping and must never reach Git")
		Expect(committed).NotTo(ContainSubstring(kubectlLastAppliedAnnotation))
		Expect(committed).NotTo(ContainSubstring("managedFields"))
		Expect(committed).NotTo(ContainSubstring("resourceVersion"))
	})

	// The race the applier claim exists for (docs/design/apply-trigger.md): two API
	// edits to one field a few seconds apart. With automated sync, Argo CD synced the
	// first edit's commit over the second edit. Under the standard nothing syncs
	// GitOps Reverser's own commits at all (an API edit asks for no apply), so the
	// later value survives, Git ends on it with no further commit, and Argo CD,
	// refreshed by its webhook on each push, only reports the Application Synced.
	It("keeps the later of two quick API edits, with no further commit and no sync", func() {
		By("committing a third IceCreamOrder from the Git side, synced by a ForeignCommit")
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.editedOrderName,
			Namespace:    testNs,
			CustomerName: "Dana",
			Container:    "Cone",
			Scoops:       []iceCreamScoop{{Flavor: "Vanilla", Quantity: 2}},
			Toppings:     []string{"Sprinkles"},
		})
		Expect(run.commitAllAndPush("argo bi-directional: add the order two edits will race on")).To(Succeed())
		run.waitForReverserSync(run.gitHEAD(), "ForeignCommit")
		run.waitForOrderContainer(run.editedOrderName, "Cone")
		baseline := run.waitForStableRemoteCommitCount(biStableCountMediumWait)
		lastSync := run.operationFinishedAt()

		By("editing the order through the API, and again seconds after that edit is pushed")
		run.patchOrderContainer(run.editedOrderName, "Cup")
		run.waitForCommittedOrderContainer(run.editedOrderName, "Cup")
		run.patchOrderContainer(run.editedOrderName, "WaffleBowl")
		run.waitForCommittedOrderContainer(run.editedOrderName, "WaffleBowl")

		By("the later value survives, with no commit after it, and Argo CD is never asked to sync")
		Consistently(func(g Gomega) {
			g.Expect(run.orderContainer(run.editedOrderName)).To(Equal("WaffleBowl"))
			g.Expect(run.gitPull()).To(Succeed())
			count, err := run.gitMainCommitCount()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(count).To(Equal(baseline+2),
				"a sync of the first edit's commit over the second edit would show as more commits")
			g.Expect(run.operationFinishedAt()).To(Equal(lastSync),
				"GitOps Reverser's own commits ask Argo CD for no sync: an API edit asks for no apply")
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("and Argo CD, which only compared, finds Git and the cluster in agreement")
		Eventually(func(g Gomega) {
			g.Expect(run.syncStatus(g)).To(Equal("Synced"))
		}, argoSyncTimeout, biPollInterval).Should(Succeed())
	})

	// The standard is full control, so the applier claim holds automated sync off: switching it
	// on (as Argo CD's Enable Auto-Sync does) is undone at the applier claim's next read, with an
	// Event, and a Git-side push after that is synced by GitOps Reverser, not by Argo CD.
	It("switches automated sync off again when somebody switches it on", func() {
		By("switching automated sync on: a client-side apply that drops enabled from the manifest")
		automated := run.baseAppConfig()
		automated.Automated = true
		run.applyArgoApp(automated)
		Expect(run.automatedEnabled()).To(BeEmpty(), "the apply removed the switch, so automated sync is on")

		// The applier is read on the applier claim's reconcile, not watched; ask for one now.
		requestClaimReconcile(testNs, argoCDClaimResource, run.appName)
		Eventually(func(g Gomega) {
			g.Expect(run.automatedEnabled()).To(Equal("false"))
			g.Expect(claimEventMessages(g, testNs, "ArgoCDClaim", run.appName, "ClaimedResourceReconciled")).
				To(ContainSubstring("was switched on outside GitOps Reverser, which switched it off again"))
		}, biEventuallyTimeout, biPollInterval).Should(Succeed())
		verifyResourceCondition(argoCDClaimResource, run.appName, testNs, "InControl", "True", "AutomatedSyncOff",
			"held off by GitOps Reverser")
		Expect(run.applicationManagers()).To(ContainElement(argoReverserInitiator),
			"the switch is held under GitOps Reverser's field manager")

		By("pushing a Git-side change, which GitOps Reverser syncs as a ForeignCommit")
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.automatedOrderName,
			Namespace:    testNs,
			CustomerName: "Bob",
			Container:    "Cone",
			Scoops:       []iceCreamScoop{{Flavor: "Strawberry", Quantity: 1}},
			Toppings:     []string{"WhippedCream"},
		})
		Expect(run.commitAllAndPush("argo bi-directional: a push after automated sync was switched on")).To(Succeed())
		head := run.gitHEAD()
		run.waitForReverserSync(head, "ForeignCommit")
		run.waitForOrderContainer(run.automatedOrderName, "Cone")
	})

	// An Application an ApplicationSet generates is rewritten from the ApplicationSet's
	// template on every reconcile, so the switch is the template's. The applier claim never writes
	// it: with automated sync on in the template it refuses the Application with a message
	// that says what to change, and once the template has it off it holds nothing and is
	// Ready.
	It("never writes the switch of an Application an ApplicationSet generates", func() {
		By("giving the ApplicationSet's folder something to apply")
		Expect(run.gitPull()).To(Succeed())
		configMap := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\n"+
			"data:\n  generated: \"true\"\n", run.generatedAppName, testNs)
		Expect(os.MkdirAll(run.repoPath(run.appSetPath), 0o755)).To(Succeed())
		Expect(os.WriteFile(run.repoPath(run.appSetPath, "configmap.yaml"), []byte(configMap), 0o644)).To(Succeed())
		Expect(run.commitAllAndPush("argo bi-directional: the ApplicationSet's folder")).To(Succeed())

		By("generating the Application with automated sync on in the template")
		DeferCleanup(func() {
			cleanupNamespacedResource(run.argoNs, "applicationset.argoproj.io", run.appSetName)
			cleanupNamespacedResource(testNs, argoCDClaimResource, run.appSetClaimName)
			cleanupGitTarget(run.appSetTargetName, testNs)
		})
		run.applyApplicationSet(true)
		Eventually(func(g Gomega) {
			owner, err := kubectlRunInNamespace(run.argoNs, "get", "application.argoproj.io", run.generatedAppName,
				"-o", "jsonpath={.metadata.ownerReferences[?(@.controller==true)].kind}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(owner).To(Equal("ApplicationSet"))
		}, argoSyncTimeout, biPollInterval).Should(Succeed())

		By("naming the generated Application from a GitTarget of its own")
		createGitTarget(run.appSetTargetName, testNs, run.gitProviderName, run.appSetPath, "main")
		createArgoCDClaim(testNs, run.appSetClaimName, run.argoNs, run.generatedAppName)
		nameApplierClaim(testNs, run.appSetTargetName, "ArgoCDClaim", run.appSetClaimName, "")

		verifyResourceCondition(argoCDClaimResource, run.appSetClaimName, testNs, "Configured", "False",
			"OwnedByApplicationSet", "is generated by ApplicationSet "+run.argoNs+"/"+run.appSetName)
		verifyResourceCondition("gittarget", run.appSetTargetName, testNs, "ApplierClaimReady", "False",
			"OwnedByApplicationSet", "spec.template.spec.syncPolicy.automated.enabled")
		Expect(run.applicationManagersOf(run.generatedAppName)).NotTo(ContainElement(argoReverserInitiator),
			"the switch of a generated Application is never written")

		By("switching automated sync off in the template, which the applier claim then only checks")
		run.applyApplicationSet(false)
		Eventually(func(g Gomega) {
			enabled, err := kubectlRunInNamespace(run.argoNs, "get", "application.argoproj.io", run.generatedAppName,
				"-o", "jsonpath={.spec.syncPolicy.automated.enabled}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(enabled)).To(Equal("false"))
		}, argoSyncTimeout, biPollInterval).Should(Succeed())
		requestClaimReconcile(testNs, argoCDClaimResource, run.appSetClaimName)
		verifyResourceCondition(argoCDClaimResource, run.appSetClaimName, testNs, "InControl", "True",
			"AutomatedSyncOff", "kept off by ApplicationSet "+run.argoNs+"/"+run.appSetName)
		verifyResourceCondition("gittarget", run.appSetTargetName, testNs, "ApplierClaimReady", "True",
			"Succeeded", "")
		Expect(run.applicationManagersOf(run.generatedAppName)).NotTo(ContainElement(argoReverserInitiator))
		finalizers, err := kubectlRunInNamespace(testNs, "get", argoCDClaimResource, run.appSetClaimName,
			"-o", "jsonpath={.metadata.finalizers}")
		Expect(err).NotTo(HaveOccurred())
		Expect(finalizers).To(BeEmpty(), "an applier claim that holds no switch has nothing to hand back")
	})
})

// A refused edit on a target with onRefusal: Reapply is reverted by an Argo CD sync,
// as it is by a Flux Kustomization in refusal_reapply_e2e_test.go, and with the same
// refusal: corpus shape 8, a live edit to an env var a kustomize base owns, which no
// declaration in the overlay can express. Its own repository, Application and
// namespace, because a second Application on the first spec's path would fight it
// over the tracking-id.
var _ = Describe("Bi Directional (Argo CD) refusal", Label("bi-directional", "argocd"), Ordered, func() {
	const overlayPath = "apps/checkout/overlays/prod"
	var (
		run    argoBiDirectionalRun
		testNs string
		repo   *RepoArtifacts
	)

	BeforeAll(func() {
		skipUnlessBiDirectionalEnabled()
		requireArgoCDInstalled()
		grantAppliersInAnyNamespace()

		testNs = testNamespaceFor("argo-refusal")
		_, _ = kubectlRun("create", "namespace", testNs)
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-argo-refusal-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		run = newArgoBiDirectionalRun(testNs, repo, "argo-refusal")
		run.livePath = overlayPath
		run.applyArgoRepoSecret()
		createGitProviderWithURLInNamespace(run.gitProviderName, testNs, repo.GitSecretHTTP, run.repoURL)
		verifyResourceStatus("gitprovider", run.gitProviderName, testNs, "True", "Succeeded", "")
	})

	AfterAll(func() {
		if !biDirectionalEnabled() {
			return
		}
		cleanupNamespacedResource(run.argoNs, "application.argoproj.io", run.appName)
		cleanupNamespacedResource(run.argoNs, "secret", run.repoSecretName)
		cleanupWatchRule(run.watchRuleName, testNs)
		cleanupGitTarget(run.gitTargetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", run.gitProviderName)
		cleanupNamespace(testNs)
	})

	It("reverts a refused edit with an Argo CD sync", func() {
		By("seeding a base-and-overlay folder that renders into this test's namespace")
		seedFolder := writeBaseOwnedFieldFolder(testNs)
		DeferCleanup(func() { _ = os.RemoveAll(seedFolder) })
		seedRenderedFolderIntoRepo(repo, testNs, seedFolder, "apps/checkout")
		Expect(run.gitPull()).To(Succeed())

		// Before any GitTarget names it, the Application is deployed the way a human
		// would: one manual sync. Every sync after this one is GitOps Reverser's.
		By("deploying the overlay once with a manual sync, automated sync off")
		app := run.baseAppConfig()
		app.Kustomize = true
		run.applyArgoApp(app)
		run.syncToRevision(remoteHeadSHA(Default, repo.CheckoutDir))
		Eventually(func(g Gomega) {
			g.Expect(checkoutLogLevel(g, testNs)).To(Equal("info"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("drifting the base-owned env var away from what Git says")
		_, err := kubectlRunInNamespace(testNs, "set", "env", "deployment/checkout", "LOG_LEVEL=debug")
		Expect(err).NotTo(HaveOccurred(), "failed to drift the base-owned env var")
		emptyBefore := emptyCommitCount(repo.CheckoutDir)

		By("creating the GitTarget on the overlay, naming the Application and asking it to reapply")
		createGitTarget(run.gitTargetName, testNs, run.gitProviderName, overlayPath, "main")
		createArgoCDClaim(testNs, run.appName, run.argoNs, run.appName)
		nameApplierClaim(testNs, run.gitTargetName, "ArgoCDClaim", run.appName, "Reapply")
		verifyResourceCondition("gittarget", run.gitTargetName, testNs, "ApplierClaimReady", "True", "Succeeded", "")
		Expect(applyFromTemplate("test/e2e/templates/manager/watchrule-resources.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
			Resources       string
		}{Name: run.watchRuleName, Namespace: testNs, DestinationName: run.gitTargetName,
			Resources: `"deployments"`},
			testNs)).To(Succeed(), "failed to apply the WatchRule")

		By("the write is refused at the boundary, and Argo CD is asked to sync the tip")
		waitForGitTargetGitPathRefused(run.gitTargetName, testNs, "WriteBoundaryRefused")
		run.waitForReverserSync("", "Refusal")
		_, pinned := run.lastOperation(Default)
		Expect(pinned).To(Equal(remoteHeadSHA(Default, repo.CheckoutDir)), "the sync applies the tip")
		Eventually(func(g Gomega) {
			g.Expect(checkoutLogLevel(g, testNs)).To(Equal("info"),
				"a refused edit on a Reapply target must be reverted by its Application")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		Expect(emptyCommitCount(repo.CheckoutDir)).To(Equal(emptyBefore),
			"the request goes to the applier; nothing moves the branch for it")
	})
})

// requireArgoCDInstalled fails fast with an actionable message. A missing Argo CD
// otherwise surfaces as an opaque "no matches for kind Application".
func requireArgoCDInstalled() {
	GinkgoHelper()
	ns := argoNamespace()
	_, err := kubectlRunInNamespace(ns, "get", "deployment", "argocd-server")
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf(
		"Argo CD is not installed in namespace %q. Run `task test-e2e-bi-directional`, "+
			"which installs it via the _argocd-installed Taskfile node.", ns))
}

func argoNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("ARGOCD_NAMESPACE")); ns != "" {
		return ns
	}
	return "argocd"
}

func newArgoBiDirectionalRun(testNs string, repo *RepoArtifacts, prefix string) argoBiDirectionalRun {
	testID := strconv.FormatInt(time.Now().UnixNano(), 10)
	return argoBiDirectionalRun{
		gitCheckout:        newGitCheckout(repo, testNs),
		testID:             testID,
		testNs:             testNs,
		argoNs:             argoNamespace(),
		repoName:           repo.RepoName,
		repoURL:            repo.RepoURLHTTP,
		appName:            fmt.Sprintf("%s-app-%s", prefix, testID),
		repoSecretName:     fmt.Sprintf("%s-repo-%s", prefix, testID),
		gitProviderName:    fmt.Sprintf("%s-provider-%s", prefix, testID),
		gitTargetName:      fmt.Sprintf("%s-target-%s", prefix, testID),
		watchRuleName:      fmt.Sprintf("%s-watchrule-%s", prefix, testID),
		receiverSecretName: fmt.Sprintf("%s-receiver-%s", prefix, testID),
		livePath:           fmt.Sprintf("argo-bi-directional/%s/live", testID),
		syncedOrderName:    fmt.Sprintf("argo-alice-order-%s", testID),
		editedOrderName:    fmt.Sprintf("argo-dana-order-%s", testID),
		automatedOrderName: fmt.Sprintf("argo-bob-order-%s", testID),
		appSetPath:         fmt.Sprintf("argo-bi-directional/%s/appset", testID),
		appSetName:         fmt.Sprintf("%s-appset-%s", prefix, testID),
		appSetTargetName:   fmt.Sprintf("%s-appset-target-%s", prefix, testID),
		generatedAppName:   fmt.Sprintf("%s-generated-%s", prefix, testID),
		appSetClaimName:    fmt.Sprintf("%s-generated-%s", prefix, testID),
	}
}

func (r argoBiDirectionalRun) baseAppConfig() argoAppConfig {
	return argoAppConfig{
		Name:                 r.appName,
		ArgoNamespace:        r.argoNs,
		RepoURL:              r.repoURL,
		Branch:               "main",
		Path:                 r.livePath,
		DestinationNamespace: r.testNs,
	}
}

// expectedTrackingID mirrors Argo's BuildAppInstanceValue:
// "<app>:<group>/<Kind>:<namespace>/<name>".
func (r argoBiDirectionalRun) expectedTrackingID(orderName string) string {
	return fmt.Sprintf("%s:%s/IceCreamOrder:%s/%s", r.appName, crdGroupArgoBiDirectional, r.testNs, orderName)
}

func (r argoBiDirectionalRun) waitForCRDEstablished() {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		output, err := kubectlRun(
			"get", "crd", iceCreamCRDName(crdGroupArgoBiDirectional),
			"-o", "jsonpath={.status.conditions[?(@.type=='Established')].status}",
		)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("True"))
	}, 20*time.Second, biPollInterval).Should(Succeed())
}

// startReverserPipeline brings up GitProvider (with a receiver Gitea pushes to) ->
// GitTarget (naming the Application as its applier) -> WatchRule on the shared live
// path. Called after the branch and the Application exist. It returns the receiver
// hook's id.
func (r argoBiDirectionalRun) startReverserPipeline() int64 {
	GinkgoHelper()
	createGitProviderWithURLInNamespace(
		r.gitProviderName, r.testNs, argoBiDirectionalRepo.GitSecretHTTP, r.repoURL)
	verifyResourceStatus("gitprovider", r.gitProviderName, r.testNs, "True", "Succeeded", "")
	hookID := registerOperatorReceiver(r.testNs, r.receiverSecretName, r.repoName)

	createGitTarget(r.gitTargetName, r.testNs, r.gitProviderName, r.livePath, "main")
	createArgoCDClaim(r.testNs, r.appName, r.argoNs, r.appName)
	nameApplierClaim(r.testNs, r.gitTargetName, "ArgoCDClaim", r.appName, "")
	verifyResourceCondition("gittarget", r.gitTargetName, r.testNs, "ApplierClaimReady", "True", "Succeeded", "")

	Expect(applyFromTemplate("test/e2e/templates/watchrule-crd.tmpl", struct {
		Name            string
		Namespace       string
		DestinationName string
		Group           string
	}{
		Name:            r.watchRuleName,
		Namespace:       r.testNs,
		DestinationName: r.gitTargetName,
		Group:           crdGroupArgoBiDirectional,
	}, r.testNs)).To(Succeed(), "failed to apply the IceCreamOrder WatchRule")

	verifyResourceStatus("gittarget", r.gitTargetName, r.testNs, "True", "Succeeded", "")
	verifyResourceStatus("watchrule", r.watchRuleName, r.testNs, "True", "Succeeded", "")

	// Gate on StreamsRunning so the IceCreamOrder watch is live before Argo CD
	// applies anything; otherwise the first live object could be folded into an
	// unattributed baseline reconcile instead of arriving as its own event.
	waitForStreamsRunning(r.gitTargetName, r.testNs)
	return hookID
}

func (r argoBiDirectionalRun) applyArgoRepoSecret() {
	GinkgoHelper()
	username, password := r.readGitCredentialSecretDataDecoded()
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/argocd-repo-secret.tmpl", struct {
		Name      string
		Namespace string
		RepoURL   string
		Username  string
		Password  string
	}{
		Name:      r.repoSecretName,
		Namespace: r.argoNs,
		RepoURL:   r.repoURL,
		Username:  username,
		Password:  password,
	}, r.argoNs)).To(Succeed(), "failed to register the Gitea repo with Argo CD")
}

func (r argoBiDirectionalRun) applyArgoApp(cfg argoAppConfig) {
	GinkgoHelper()
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/argocd-application.tmpl", cfg, r.argoNs)).
		To(Succeed(), "failed to apply Argo CD Application")
}

// applyApplicationSet applies the ApplicationSet that generates generatedAppName over the
// ApplicationSet spec's folder, with automated sync on or off in its template.
func (r argoBiDirectionalRun) applyApplicationSet(automated bool) {
	GinkgoHelper()
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/argocd-applicationset.tmpl", struct {
		Name                 string
		ArgoNamespace        string
		ApplicationName      string
		RepoURL              string
		Branch               string
		Path                 string
		DestinationNamespace string
		Automated            bool
	}{
		Name:                 r.appSetName,
		ArgoNamespace:        r.argoNs,
		ApplicationName:      r.generatedAppName,
		RepoURL:              r.repoURL,
		Branch:               "main",
		Path:                 r.appSetPath,
		DestinationNamespace: r.testNs,
		Automated:            automated,
	}, r.argoNs)).To(Succeed(), "failed to apply the Argo CD ApplicationSet")
}

// automatedEnabled is the Application's spec.syncPolicy.automated.enabled as written, "" when it
// has none.
func (r argoBiDirectionalRun) automatedEnabled() string {
	GinkgoHelper()
	out, err := kubectlRunInNamespace(r.argoNs, "get", "application.argoproj.io", r.appName,
		"-o", "jsonpath={.spec.syncPolicy.automated.enabled}")
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// applicationManagers lists the field managers of the Application.
func (r argoBiDirectionalRun) applicationManagers() []string {
	GinkgoHelper()
	return r.applicationManagersOf(r.appName)
}

// applicationManagersOf lists the field managers of the Application name.
func (r argoBiDirectionalRun) applicationManagersOf(name string) []string {
	GinkgoHelper()
	out, err := kubectlRunInNamespace(r.argoNs, "get", "application.argoproj.io", name,
		"--show-managed-fields", "-o", "jsonpath={.metadata.managedFields[*].manager}")
	Expect(err).NotTo(HaveOccurred())
	return strings.Fields(out)
}

// claimEventMessages is the messages of the Events with reason on the applier claim kind/name.
func claimEventMessages(g Gomega, namespace, kind, name, reason string) string {
	out, err := kubectlRunInNamespace(namespace, "get", "events", "--field-selector",
		"involvedObject.kind="+kind+",involvedObject.name="+name+",reason="+reason,
		"-o", "jsonpath={.items[*].message}")
	g.Expect(err).NotTo(HaveOccurred())
	return out
}

// configureArgoWebhookSecret sets the Gogs webhook secret on the cluster-global
// argocd-secret. argocd-server watches that Secret and reloads within seconds; a
// merge patch leaves the server-generated TLS keys and session key untouched.
// Idempotent, so re-running the corner on a warm cluster is harmless.
func (r argoBiDirectionalRun) configureArgoWebhookSecret() {
	GinkgoHelper()
	patch := fmt.Sprintf(`{"stringData":{"webhook.gogs.secret":%q}}`, argoWebhookSecret)
	_, err := kubectlRunInNamespace(r.argoNs, "patch", "secret", "argocd-secret",
		"--type=merge", "--patch", patch)
	Expect(err).NotTo(HaveOccurred(), "failed to set webhook.gogs.secret on argocd-secret")
}

// ensureArgoWebhook registers a Gitea webhook that notifies argocd-server on push,
// so Argo refreshes immediately instead of waiting for its timed reconciliation.
// With automated sync off, a refresh only compares: the specs show it never syncs.
//
// The match is exact host+path: Argo compiles a regex from the payload's
// repository HTMLURL (derived from Gitea's ROOT_URL) and tests it against the
// Application's spec.source.repoURL. Gitea's ROOT_URL host equals the in-cluster
// repo host, and the regex tolerates the :13000 port and .git suffix, so it hits.
func (r argoBiDirectionalRun) ensureArgoWebhook() {
	GinkgoHelper()
	gitea := giteaTestInstance()
	ctx, cancel := gitea.Context()
	defer cancel()

	// argocd-server serves plain HTTP on :80 (server.insecure=true).
	callbackURL := fmt.Sprintf("http://argocd-server.%s.svc.cluster.local/api/webhook", r.argoNs)

	// Idempotent: drop any prior Argo-targeted hook before recreating it.
	hooks, err := gitea.Client().ListRepoHooks(ctx, gitea.Org, r.repoName)
	Expect(err).NotTo(HaveOccurred(), "failed to list repo webhooks")
	for _, hook := range hooks {
		if strings.Contains(hook.Config.URL, "/api/webhook") {
			Expect(gitea.Client().DeleteRepoHook(ctx, gitea.Org, r.repoName, hook.ID)).
				To(Succeed(), "failed to delete a stale Argo CD webhook")
		}
	}

	_, err = gitea.Client().CreateGiteaWebhook(
		ctx, gitea.Org, r.repoName, callbackURL, argoWebhookSecret, []string{"push"})
	Expect(err).NotTo(HaveOccurred(), "failed to create the Gitea -> Argo CD webhook")
}

// syncToRevision is a human pressing Sync: it sets the Application's `.operation`
// to a sync of revision, initiated by "e2e", the kubectl-only equivalent of
// `argocd app sync --revision <sha>`. Only the refusal spec's setup uses it, before
// any GitTarget names the Application.
func (r argoBiDirectionalRun) syncToRevision(revision string) {
	GinkgoHelper()
	By(fmt.Sprintf("syncing Argo CD Application %q to revision %s by hand", r.appName, revision[:8]))

	// Argo clears `.operation` when it finishes and leaves the result in
	// `.status.operationState`. Without pinning the previous finishedAt, a
	// re-sync of an already-synced revision would match the PREVIOUS operation's
	// Succeeded/revision immediately and assert nothing at all.
	previousFinishedAt := r.operationFinishedAt()

	patch := fmt.Sprintf(
		`{"operation":{"initiatedBy":{"username":"e2e"},"sync":{"revision":%q}}}`, revision)
	Eventually(func(g Gomega) {
		_, err := kubectlRunInNamespace(r.argoNs, "patch", "application.argoproj.io", r.appName,
			"--type=merge", "--patch", patch)
		// A sync already in flight rejects a new `.operation`; retry until it lands.
		g.Expect(err).NotTo(HaveOccurred())
	}, argoSyncTimeout, biPollInterval).Should(Succeed())

	Eventually(func(g Gomega) {
		app, err := r.appObject()
		g.Expect(err).NotTo(HaveOccurred())
		finishedAt, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "finishedAt")
		g.Expect(finishedAt).NotTo(Equal(previousFinishedAt), "Argo CD has not finished a NEW sync operation yet")
		phase, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "phase")
		g.Expect(phase).To(Equal("Succeeded"), "Argo CD sync operation did not succeed")
		synced, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "syncResult", "revision")
		g.Expect(synced).To(Equal(revision), "Argo CD synced a different revision")
	}, argoSyncTimeout, biPollInterval).Should(Succeed())
}

// waitForReverserSync waits for a sync GitOps Reverser started, for reason, to
// succeed: initiated by gitops-reverser with automated: true, carrying the reason in
// its info, and pinned to revision (an empty revision accepts any commit), which is
// also the revision Argo CD synced.
func (r argoBiDirectionalRun) waitForReverserSync(revision, reason string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		app, err := r.appObject()
		g.Expect(err).NotTo(HaveOccurred())
		state, found, _ := unstructured.NestedMap(app.Object, "status", "operationState")
		g.Expect(found).To(BeTrue(), "Argo CD has run no operation yet")

		username, _, _ := unstructured.NestedString(state, "operation", "initiatedBy", "username")
		automated, _, _ := unstructured.NestedBool(state, "operation", "initiatedBy", "automated")
		g.Expect(username).To(Equal(argoReverserInitiator), "the last sync was not GitOps Reverser's")
		g.Expect(automated).To(BeTrue(), "GitOps Reverser's syncs stand in for automated sync")
		g.Expect(operationInfo(state, "gitops-reverser reason")).To(Equal(reason))

		pinned, _, _ := unstructured.NestedString(state, "operation", "sync", "revision")
		synced, _, _ := unstructured.NestedString(state, "syncResult", "revision")
		g.Expect(pinned).NotTo(BeEmpty(), "a sync GitOps Reverser starts is always pinned")
		if revision != "" {
			g.Expect(pinned).To(Equal(revision), "the sync must be pinned to the commit GitOps Reverser checked")
		}
		g.Expect(synced).To(Equal(pinned), "Argo CD synced a revision other than the pinned one")

		phase, _, _ := unstructured.NestedString(state, "phase")
		g.Expect(phase).To(Equal("Succeeded"), "the sync GitOps Reverser asked for did not succeed")
		resources, _, _ := unstructured.NestedSlice(state, "syncResult", "resources")
		g.Expect(resources).NotTo(BeEmpty(), "Argo CD synced zero resources — is the source path empty?")
	}, argoRequestedSyncTimeout, biPollInterval).Should(Succeed())
}

// operationInfo is the value of the named entry in an operation state's info.
func operationInfo(state map[string]any, name string) string {
	info, _, _ := unstructured.NestedSlice(state, "operation", "info")
	for _, item := range info {
		entry, ok := item.(map[string]any)
		if ok && entry["name"] == name {
			value, _ := entry["value"].(string)
			return value
		}
	}
	return ""
}

// lastOperation is who started the Application's last operation, and the revision
// it was pinned to; both empty when it has run none.
func (r argoBiDirectionalRun) lastOperation(g Gomega) (string, string) {
	app, err := r.appObject()
	g.Expect(err).NotTo(HaveOccurred())
	username, _, _ := unstructured.NestedString(app.Object,
		"status", "operationState", "operation", "initiatedBy", "username")
	revision, _, _ := unstructured.NestedString(app.Object,
		"status", "operationState", "operation", "sync", "revision")
	return username, revision
}

// claimOperation is the ArgoCDClaim's status.operation: the sync GitOps Reverser
// last started on the Application.
func (r argoBiDirectionalRun) claimOperation(g Gomega) claimOperation {
	return readClaimOperation(g, r.testNs, argoCDClaimResource, r.appName)
}

// operationFinishedAt returns "" when the Application has never run an operation.
func (r argoBiDirectionalRun) operationFinishedAt() string {
	output, err := kubectlRunInNamespace(r.argoNs, "get", "application.argoproj.io", r.appName,
		"-o", "jsonpath={.status.operationState.finishedAt}")
	if err != nil {
		return "" // Application not created yet.
	}
	return strings.TrimSpace(output)
}

// The read helpers below return errors rather than calling Expect. They run inside
// Eventually/Consistently bodies, where a bare Expect would fail the spec on the
// first poll instead of retrying — which is exactly how a slow Argo sync would be
// misdiagnosed as a missing object.

func (r argoBiDirectionalRun) appObject() (*unstructured.Unstructured, error) {
	output, err := kubectlRunInNamespace(r.argoNs, "get", "application.argoproj.io", r.appName, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("read Argo CD Application %q: %w", r.appName, err)
	}

	var obj unstructured.Unstructured
	if err := json.Unmarshal([]byte(output), &obj.Object); err != nil {
		return nil, fmt.Errorf("parse Argo CD Application %q: %w", r.appName, err)
	}
	return &obj, nil
}

func (r argoBiDirectionalRun) syncStatus(g Gomega) string {
	app, err := r.appObject()
	g.Expect(err).NotTo(HaveOccurred())
	status, _, _ := unstructured.NestedString(app.Object, "status", "sync", "status")
	return status
}

func (r argoBiDirectionalRun) liveOrder(name string) (*unstructured.Unstructured, error) {
	output, err := kubectlRunInNamespace(
		r.testNs, "get", iceCreamCRDName(crdGroupArgoBiDirectional), name, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("read live IceCreamOrder %q: %w", name, err)
	}

	var obj unstructured.Unstructured
	if err := json.Unmarshal([]byte(output), &obj.Object); err != nil {
		return nil, fmt.Errorf("parse live IceCreamOrder %q: %w", name, err)
	}
	return &obj, nil
}

func (r argoBiDirectionalRun) liveOrderAnnotations(name string) map[string]string {
	GinkgoHelper()
	order, err := r.liveOrder(name)
	Expect(err).NotTo(HaveOccurred())

	annotations, _, err := unstructured.NestedStringMap(order.Object, "metadata", "annotations")
	Expect(err).NotTo(HaveOccurred())
	return annotations
}

func (r argoBiDirectionalRun) orderContainer(name string) (string, error) {
	order, err := r.liveOrder(name)
	if err != nil {
		return "", err
	}
	value, found, err := unstructured.NestedString(order.Object, "spec", "container")
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("IceCreamOrder %q has no spec.container", name)
	}
	return value, nil
}

func (r argoBiDirectionalRun) waitForOrderContainer(name, container string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		value, err := r.orderContainer(name)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(value).To(Equal(container))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

// waitForCommittedOrderContainer waits for the order's committed file to hold container.
func (r argoBiDirectionalRun) waitForCommittedOrderContainer(name, container string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())
		g.Expect(r.readCommittedOrder(name)).To(ContainSubstring("container: " + container))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r argoBiDirectionalRun) patchOrderContainer(name, container string) {
	GinkgoHelper()
	_, err := kubectlRunInNamespace(r.testNs, "patch", iceCreamCRDName(crdGroupArgoBiDirectional), name,
		"--type=merge", "--patch", fmt.Sprintf(`{"spec":{"container":%q}}`, container))
	Expect(err).NotTo(HaveOccurred(), "failed to patch IceCreamOrder through the API")
}

func (r argoBiDirectionalRun) liveOrderPath(name string) string {
	return r.repoPath(r.livePath, iceCreamInstancePath(crdGroupArgoBiDirectional, r.testNs, name))
}

func (r argoBiDirectionalRun) writeLiveOrder(order iceCreamOrderFile) {
	GinkgoHelper()
	Expect(r.gitPull()).To(Succeed(), "the Git-side change is made on the current tip")
	content, err := renderTemplate("test/e2e/templates/icecreamorder-instance.tmpl", struct {
		Name         string
		Namespace    string
		Labels       map[string]string
		Annotations  map[string]string
		CustomerName string
		Container    string
		Scoops       []iceCreamScoop
		Toppings     []string
		Group        string
	}{
		Name:         order.Name,
		Namespace:    order.Namespace,
		CustomerName: order.CustomerName,
		Container:    order.Container,
		Scoops:       order.Scoops,
		Toppings:     order.Toppings,
		Group:        crdGroupArgoBiDirectional,
	})
	Expect(err).NotTo(HaveOccurred(), "failed to render IceCreamOrder manifest")
	Expect(os.MkdirAll(filepath.Dir(r.liveOrderPath(order.Name)), 0o755)).To(Succeed())
	Expect(os.WriteFile(r.liveOrderPath(order.Name), []byte(content), 0o644)).To(Succeed())
}

func (r argoBiDirectionalRun) readCommittedOrder(name string) string {
	GinkgoHelper()
	content, err := os.ReadFile(r.liveOrderPath(name))
	Expect(err).NotTo(HaveOccurred(), "expected a committed IceCreamOrder at %s", r.liveOrderPath(name))
	return string(content)
}
