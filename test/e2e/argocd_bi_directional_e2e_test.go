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
// Everything here is driven through kubectl — refresh via the
// `argocd.argoproj.io/refresh` annotation, sync via the Application's `.operation`
// field — so the devcontainer needs no `argocd` binary.
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

	// Self-heal reacts to live drift on a ~2s backoff, so 90s is generous. The
	// spec never waits on Argo's 180s timed refresh (see setup/argocd/values.yaml).
	argoSelfHealTimeout = 90 * time.Second
	argoSyncTimeout     = 120 * time.Second
	// Bounded so it can only pass via the webhook: comfortably longer than webhook
	// delivery + refresh + auto-sync, but well under Argo's 180s timed
	// reconciliation. If the webhook path is broken, this times out.
	argoWebhookSyncTimeout = 20 * time.Second // measured ~4s end-to-end in the corner runs
	// Long enough that a self-heal would certainly have fired if the field were not ignored.
	argoNoSelfHealWindow = 10 * time.Second // measured: self-heal reverts a non-ignored field in ~1s
)

// argoBiDirectionalRepo holds the file-local repo fixtures for this describe block.
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

	gitProviderName string
	gitTargetName   string
	watchRuleName   string
	livePath        string

	// One order per phase, so the phases never contend for the same object.
	syncedOrderName      string
	selfHealOrderName    string
	ignoredOrderName     string
	recommendedOrderName string
}

// argoAppConfig is the knob set the spec turns between phases. The Application is
// re-applied (not patched) each time, so dropping a field really removes it.
type argoAppConfig struct {
	Name                 string
	ArgoNamespace        string
	RepoURL              string
	Branch               string
	Path                 string
	DestinationNamespace string

	Automated bool
	Prune     bool
	SelfHeal  bool

	SyncOptions        []string
	IgnoreGroup        string
	IgnoreKind         string
	IgnoreJSONPointers []string
}

var _ = Describe("Bi Directional (Argo CD)", Label("bi-directional", "argocd"), Ordered, func() {
	var run argoBiDirectionalRun
	var testNs string

	BeforeAll(func() {
		skipUnlessBiDirectionalEnabled()
		requireArgoCDInstalled()

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

		run = newArgoBiDirectionalRun(testNs)
		run.assertCheckoutReady()

		By("installing the IceCreamOrder CRD directly")
		// Unlike the Flux spec, the CRD does not travel through Git here: Argo's
		// role in this spec is to apply the *live path*, and a CRD-then-CR ordering
		// test would only re-cover what the Flux spec already covers.
		Expect(applyIceCreamCRD(crdGroupArgoBiDirectional)).To(Succeed())
		run.waitForCRDEstablished()

		By("registering the Gitea repository with Argo CD")
		run.applyArgoRepoSecret()

		By("wiring a Gitea -> Argo CD webhook so pushes reconcile without waiting for the poll")
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
		cleanupWatchRule(run.watchRuleName, testNs)
		cleanupGitTarget(run.gitTargetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", run.gitProviderName)
		cleanupClusterResource("crd", iceCreamCRDName(crdGroupArgoBiDirectional))
		cleanupNamespace(testNs)
	})

	It("should keep Argo CD's bookkeeping out of Git, and prove selfHeal on vs off is the difference "+
		"between losing and keeping a shared-field edit", func() {
		// Ordering is load-bearing, and both halves of it are.
		//
		// 1. SetupRepo leaves the Gitea repo EMPTY. There is no `main` on the
		//    remote until something pushes one, and `git pull` cannot fetch a ref
		//    that does not exist. So seed the branch first.
		//
		// 2. The reverser's first reconcile makes its managed path match LIVE
		//    state: it writes its bootstrap files and DELETES anything it does not
		//    own. An IceCreamOrder committed before that reconcile is pruned before
		//    Argo CD ever sees it — Argo then syncs an empty path and truthfully
		//    reports Succeeded, having applied nothing.
		//
		// So: seed the branch, let the reverser settle, and only then write orders.
		// The Flux spec depends on exactly the same sequence.
		By("seeding the branch so the GitTarget has a main to bootstrap")
		Expect(run.commitAllAndPush("argo bi-directional: seed main")).To(Succeed())

		By("enabling gitops-reverser on the shared live path")
		run.startReverserPipeline()

		// Absorbs the reverser's bootstrap/prune commit, so the zero-churn
		// assertions below measure Argo CD's effect and nothing else.
		baselineCommitCount := run.waitForStableRemoteCommitCount(biStableCountShortWait)

		// ---------------------------------------------------------------------
		// Phase 1 — Argo CD's stamps must not reach Git.
		//
		// Argo's repo-server stamps `argocd.argoproj.io/tracking-id` onto every
		// non-CRD manifest it renders, and its default client-side apply writes
		// `last-applied-configuration`. Both land on the LIVE object, which is
		// exactly what gitops-reverser mirrors back to Git.
		//
		// Neither is user intent, so neither may be committed. Regression guard for
		// internal/sanitize: before that fix, the tracking-id was committed, which
		// (a) polluted Git with a provenance string naming one Application, and
		// (b) armed a real failure — Argo's GetAppName never validates the id
		// against the object, so a manifest carrying a foreign tracking-id makes a
		// second Application fail to sync with "Shared resource found".
		//
		// The strongest form of the assertion is a commit COUNT: if sanitization is
		// complete, the sanitized live object equals the file already in Git, so
		// Argo syncing a reverser-watched path produces ZERO commits.
		// ---------------------------------------------------------------------
		By("committing a clean IceCreamOrder through normal GitOps")
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.syncedOrderName,
			Namespace:    testNs,
			CustomerName: "Alice",
			Container:    "Cone",
			Scoops:       []iceCreamScoop{{Flavor: "Vanilla", Quantity: 2}},
			Toppings:     []string{"Sprinkles"},
		})
		Expect(run.commitAllAndPush("argo bi-directional: add icecream order")).To(Succeed())
		syncedHead := run.gitHEAD()
		run.expectRemoteCommitCount(baselineCommitCount + 1)

		By("creating a manually-synced Argo CD Application for the shared live path")
		run.applyArgoApp(run.baseAppConfig())
		run.syncToRevision(syncedHead)
		run.waitForOrderContainer(run.syncedOrderName, "Cone")

		By("verifying Argo CD stamped its bookkeeping onto the live object")
		liveAnnotations := run.liveOrderAnnotations(run.syncedOrderName)
		Expect(liveAnnotations).To(HaveKeyWithValue(
			argoTrackingIDAnnotation, run.expectedTrackingID(run.syncedOrderName)),
			"Argo CD's default resource-tracking method should stamp the tracking-id annotation")
		Expect(liveAnnotations).To(HaveKey(kubectlLastAppliedAnnotation),
			"Argo CD's default apply is client-side, which writes last-applied-configuration")

		By("verifying none of that bookkeeping is committed, and no commit happens at all")
		// If any of it leaked, the sanitized live object would differ from the file
		// already in Git and the reverser would commit — so the count would grow.
		run.consistentlyExpectRemoteCommitCount(baselineCommitCount+1, biStableCountLongWait)

		committed := run.readCommittedOrder(run.syncedOrderName)
		Expect(committed).NotTo(ContainSubstring(argoTrackingIDAnnotation),
			"Argo CD's tracking-id is controller bookkeeping and must never reach Git")
		Expect(committed).NotTo(ContainSubstring(kubectlLastAppliedAnnotation))
		Expect(committed).NotTo(ContainSubstring("managedFields"))
		Expect(committed).NotTo(ContainSubstring("resourceVersion"))

		By("re-syncing Argo CD with no Git change and expecting no churn")
		run.syncToRevision(syncedHead)
		run.consistentlyExpectRemoteCommitCount(baselineCommitCount+1, biStableCountMediumWait)

		// ---------------------------------------------------------------------
		// Phase 2 — selfHeal destroys the API-side change, and Git history flaps.
		//
		// Two clocks: self-heal reverts live drift essentially immediately —
		// sub-second on the FIRST drift, because the backoff is zero on attempt 0
		// (docs/design/support-boundary/argocd-bi-directional.md) — while a new Git revision is
		// only noticed on the 180s timed refresh. So Argo replays its stale cached
		// revision long before it ever looks at what the reverser committed. The
		// user's change is lost.
		//
		// The flap is TWO commits in quick succession: the reverser captures the API
		// edit (WaffleBowl), then self-heal reverts to the Git-side value and the
		// reverser captures THAT too (Cone). This is deterministic, not a race: the
		// Kubernetes API watch the reverser runs delivers every edit in order and
		// never collapses them, and with commitWindow=0 each edit is its own commit.
		// So even though self-heal is sub-second, both edits are observed and both
		// are committed — asserted as an exact +2 delta at the end of the phase.
		// ---------------------------------------------------------------------
		By("switching Argo CD to automated sync with selfHeal enabled")
		selfHealApp := run.baseAppConfig()
		selfHealApp.Automated = true
		selfHealApp.Prune = true
		selfHealApp.SelfHeal = true
		run.applyArgoApp(selfHealApp)

		By("committing a second IceCreamOrder through normal GitOps")
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.selfHealOrderName,
			Namespace:    testNs,
			CustomerName: "Bob",
			Container:    "Cone",
			Scoops:       []iceCreamScoop{{Flavor: "Strawberry", Quantity: 1}},
			Toppings:     []string{"WhippedCream"},
		})
		Expect(run.commitAllAndPush("argo bi-directional: add self-heal icecream order")).To(Succeed())

		By("letting the Gitea -> Argo CD webhook drive the sync — no manual refresh, no manual sync")
		// The push notifies argocd-server, which refreshes the automated app and
		// auto-syncs the new commit. This lands well inside argoWebhookSyncTimeout,
		// far below Argo's 180s poll — so it can only pass via the webhook.
		Eventually(func(g Gomega) {
			value, err := run.orderContainer(run.selfHealOrderName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(value).To(Equal("Cone"))
		}, argoWebhookSyncTimeout, biPollInterval).Should(Succeed(),
			"the webhook should drive Argo CD to auto-sync the new commit")

		By("recording the commit count just before the API edit, to observe the self-heal flap")
		Expect(run.gitPull()).To(Succeed())
		preFlapCount, err := run.gitMainCommitCount()
		Expect(err).NotTo(HaveOccurred())

		By("changing that IceCreamOrder through the Kubernetes API")
		run.patchOrderContainer(run.selfHealOrderName, "WaffleBowl")

		By("verifying Argo CD self-heals the change away — the API-side edit is LOST")
		Eventually(func(g Gomega) {
			g.Expect(run.orderContainer(run.selfHealOrderName)).To(Equal("Cone"))
		}, argoSelfHealTimeout, biPollInterval).Should(Succeed(),
			"selfHeal should replay the stale cached revision over the API-side change")
		Consistently(func(g Gomega) {
			g.Expect(run.orderContainer(run.selfHealOrderName)).To(Equal("Cone"))
		}, biStableCountMediumWait, biPollInterval).Should(Succeed())

		By("verifying Git converges back to the Git-side value and then stops moving")
		Eventually(func(g Gomega) {
			g.Expect(run.gitPull()).To(Succeed())
			g.Expect(run.readCommittedOrder(run.selfHealOrderName)).To(ContainSubstring("container: Cone"))
		}, biEventuallyTimeout, biPollInterval).Should(Succeed())

		settled, err := run.gitMainCommitCount()
		Expect(err).NotTo(HaveOccurred())
		run.consistentlyExpectRemoteCommitCount(settled, biStableCountMediumWait)

		// The flap is exactly two commits, and this is deterministic — not a race.
		// gitops-reverser watches the IceCreamOrder through the Kubernetes API watch,
		// which delivers EVERY edit in order and never collapses them: it sees the
		// API edit (WaffleBowl), then self-heal's revert to the Git-side value (Cone).
		// With commitWindow=0 each observed edit finalizes as its own commit, so the
		// self-heal round trip always writes precisely two commits.
		By(fmt.Sprintf("verifying the self-heal flap wrote exactly two commits (%d -> %d)", preFlapCount, settled))
		Expect(settled-preFlapCount).To(Equal(2),
			"the watch delivers both the API edit and self-heal's revert, and each is one commit")

		// ---------------------------------------------------------------------
		// Phase 3 — the safe recipe: ignoreDifferences gives a field to the API.
		//
		// Identical to phase 2 except for `ignoreDifferences` on /spec/scoops. That
		// one stanza is the difference between losing the change and keeping it:
		// the field never registers as drift, so selfHeal never fires, and the
		// reverser publishes the API-side value to Git.
		//
		// `RespectIgnoreDifferences=true` is belt-and-braces here rather than
		// load-bearing: because the reverser keeps Git current, target and live
		// agree anyway. It closes the window where an unrelated Git change triggers
		// a sync between the API patch and the reverser's commit, which would
		// otherwise reset the ignored field.
		// ---------------------------------------------------------------------
		By("giving /spec/scoops to the API via ignoreDifferences, keeping selfHeal on")
		ignoredApp := run.baseAppConfig()
		ignoredApp.Automated = true
		ignoredApp.Prune = true
		ignoredApp.SelfHeal = true
		ignoredApp.SyncOptions = []string{"RespectIgnoreDifferences=true"}
		ignoredApp.IgnoreGroup = crdGroupArgoBiDirectional
		ignoredApp.IgnoreKind = "IceCreamOrder"
		ignoredApp.IgnoreJSONPointers = []string{"/spec/scoops"}
		run.applyArgoApp(ignoredApp)

		By("committing a third IceCreamOrder through normal GitOps")
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.ignoredOrderName,
			Namespace:    testNs,
			CustomerName: "Charlie",
			Container:    "Cup",
			Scoops:       []iceCreamScoop{{Flavor: "Vanilla", Quantity: 2}},
			Toppings:     []string{"HotFudge"},
		})
		Expect(run.commitAllAndPush("argo bi-directional: add split-ownership icecream order")).To(Succeed())

		By("letting the webhook drive the sync of the third order")
		Eventually(func(g Gomega) {
			value, err := run.orderScoopFlavor(run.ignoredOrderName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(value).To(Equal("Vanilla"))
		}, argoWebhookSyncTimeout, biPollInterval).Should(Succeed(),
			"the webhook should drive Argo CD to auto-sync the third commit")

		By("changing the ignored field through the Kubernetes API")
		run.patchOrderScoop(run.ignoredOrderName, "MintChip", 4)

		By("verifying selfHeal does NOT revert an ignored field")
		Consistently(func(g Gomega) {
			g.Expect(run.orderScoopFlavor(run.ignoredOrderName)).To(Equal("MintChip"))
		}, argoNoSelfHealWindow, biPollInterval).Should(Succeed(),
			"ignoreDifferences should keep /spec/scoops out of the drift comparison")

		By("verifying Argo CD still reports the Application as Synced")
		Expect(run.syncStatus()).To(Equal("Synced"),
			"an ignored field must not put the Application OutOfSync")

		By("verifying gitops-reverser publishes the API-side value to Git")
		Eventually(func(g Gomega) {
			g.Expect(run.gitPull()).To(Succeed())
			g.Expect(run.readCommittedOrder(run.ignoredOrderName)).To(ContainSubstring("flavor: MintChip"))
		}, biEventuallyTimeout, biPollInterval).Should(Succeed())

		By("verifying the system is at rest")
		finalCount, err := run.gitMainCommitCount()
		Expect(err).NotTo(HaveOccurred())
		run.consistentlyExpectRemoteCommitCount(finalCount, biStableCountMediumWait)
		Expect(run.orderScoopFlavor(run.ignoredOrderName)).To(Equal("MintChip"))

		// ---------------------------------------------------------------------
		// Phase 4 — the recommended shared-field loop: selfHeal:false + webhook.
		//
		// This is the ONE Argo CD configuration docs/bi-directional.md recommends
		// for a genuinely shared field (docs/design/support-boundary/argocd-bi-directional.md).
		// It is the deterministic counterpart to phase 2: phase 2 proved selfHeal
		// LOSES the API edit; this proves selfHeal:false KEEPS it, and — unlike the
		// ignoreDifferences of phase 3 — the field stays fully GitOps-driven, so a
		// Git-side change to the SAME field still reaches the cluster.
		//
		// One field, both directions:
		//   - API side: an edit to spec.container is captured to Git and, because
		//     selfHeal is off, never reverted;
		//   - Git side: a later commit to the SAME spec.container is applied back
		//     to the cluster through the webhook.
		//
		// No race here: nothing reverts live drift, so every step is deterministic
		// and can be asserted directly.
		// ---------------------------------------------------------------------
		By("switching Argo CD to automated sync with selfHeal DISABLED (the recommended shared-field mode)")
		recommendedApp := run.baseAppConfig()
		recommendedApp.Automated = true
		recommendedApp.Prune = true
		recommendedApp.SelfHeal = false
		run.applyArgoApp(recommendedApp)

		By("committing a fourth IceCreamOrder through normal GitOps")
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.recommendedOrderName,
			Namespace:    testNs,
			CustomerName: "Dana",
			Container:    "Cone",
			Scoops:       []iceCreamScoop{{Flavor: "Vanilla", Quantity: 2}},
			Toppings:     []string{"Sprinkles"},
		})
		Expect(run.commitAllAndPush("argo bi-directional: add shared-field selfHeal-off order")).To(Succeed())

		By("letting the webhook drive the sync of the fourth order")
		Eventually(func(g Gomega) {
			value, containerErr := run.orderContainer(run.recommendedOrderName)
			g.Expect(containerErr).NotTo(HaveOccurred())
			g.Expect(value).To(Equal("Cone"))
		}, argoWebhookSyncTimeout, biPollInterval).Should(Succeed(),
			"the webhook should drive Argo CD to auto-sync the fourth commit")

		// --- API side of the shared field: captured, not reverted ---
		By("changing the shared field spec.container through the Kubernetes API")
		run.patchOrderContainer(run.recommendedOrderName, "WaffleBowl")

		By("verifying selfHeal:false leaves the API edit in place (the contrast with phase 2)")
		// Long enough that a self-heal would certainly have fired had it been on;
		// the reverser committing WaffleBowl (which fires the webhook) only ever
		// converges Argo TO WaffleBowl, never reverts it.
		Consistently(func(g Gomega) {
			g.Expect(run.orderContainer(run.recommendedOrderName)).To(Equal("WaffleBowl"))
		}, argoNoSelfHealWindow, biPollInterval).Should(Succeed(),
			"with selfHeal off, Argo must not revert the live edit before the reverser captures it")

		By("verifying gitops-reverser captures the API edit to Git")
		Eventually(func(g Gomega) {
			g.Expect(run.gitPull()).To(Succeed())
			g.Expect(run.readCommittedOrder(run.recommendedOrderName)).To(ContainSubstring("container: WaffleBowl"))
		}, biEventuallyTimeout, biPollInterval).Should(Succeed())

		// --- Git side of the SAME shared field: applied back through the webhook ---
		By("changing the SAME field spec.container from the Git side and pushing")
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.recommendedOrderName,
			Namespace:    testNs,
			CustomerName: "Dana",
			// A different valid enum value from the API-side WaffleBowl and the
			// initial Cone; spec.container is constrained to {Cup, Cone, WaffleBowl}.
			Container: "Cup",
			Scoops:    []iceCreamScoop{{Flavor: "Vanilla", Quantity: 2}},
			Toppings:  []string{"Sprinkles"},
		})
		Expect(run.commitAllAndPush("argo bi-directional: git-side change to the shared field")).To(Succeed())

		By("verifying the webhook applies the Git-side change to the cluster — the field is still GitOps-driven")
		Eventually(func(g Gomega) {
			value, containerErr := run.orderContainer(run.recommendedOrderName)
			g.Expect(containerErr).NotTo(HaveOccurred())
			g.Expect(value).To(Equal("Cup"))
		}, argoWebhookSyncTimeout, biPollInterval).Should(Succeed(),
			"a Git-side change to the shared field must reach the cluster (ignoreDifferences would have blocked it)")

		By("verifying both loops settle in agreement on the shared field")
		Eventually(func(g Gomega) {
			g.Expect(run.gitPull()).To(Succeed())
			g.Expect(run.readCommittedOrder(run.recommendedOrderName)).To(ContainSubstring("container: Cup"))
		}, biEventuallyTimeout, biPollInterval).Should(Succeed())
		recommendedSettled, err := run.gitMainCommitCount()
		Expect(err).NotTo(HaveOccurred())
		run.consistentlyExpectRemoteCommitCount(recommendedSettled, biStableCountMediumWait)
		Expect(run.orderContainer(run.recommendedOrderName)).To(Equal("Cup"))
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

func newArgoBiDirectionalRun(testNs string) argoBiDirectionalRun {
	testID := strconv.FormatInt(time.Now().UnixNano(), 10)
	return argoBiDirectionalRun{
		gitCheckout:          newGitCheckout(argoBiDirectionalRepo, testNs),
		testID:               testID,
		testNs:               testNs,
		argoNs:               argoNamespace(),
		repoName:             argoBiDirectionalRepo.RepoName,
		repoURL:              argoBiDirectionalRepo.RepoURLHTTP,
		appName:              fmt.Sprintf("argo-bi-app-%s", testID),
		repoSecretName:       fmt.Sprintf("argo-bi-repo-%s", testID),
		gitProviderName:      fmt.Sprintf("argo-bi-provider-%s", testID),
		gitTargetName:        fmt.Sprintf("argo-bi-target-%s", testID),
		watchRuleName:        fmt.Sprintf("argo-bi-watchrule-%s", testID),
		livePath:             fmt.Sprintf("argo-bi-directional/%s/live", testID),
		syncedOrderName:      fmt.Sprintf("argo-alice-order-%s", testID),
		selfHealOrderName:    fmt.Sprintf("argo-bob-order-%s", testID),
		ignoredOrderName:     fmt.Sprintf("argo-charlie-order-%s", testID),
		recommendedOrderName: fmt.Sprintf("argo-dana-order-%s", testID),
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

// startReverserPipeline brings up GitProvider -> GitTarget -> WatchRule on the
// shared live path. Called after the branch exists, not in BeforeAll.
func (r argoBiDirectionalRun) startReverserPipeline() {
	GinkgoHelper()
	createGitProviderWithURLInNamespace(
		r.gitProviderName, r.testNs, argoBiDirectionalRepo.GitSecretHTTP, r.repoURL)
	verifyResourceStatus("gitprovider", r.gitProviderName, r.testNs, "True", "Succeeded", "")

	createGitTarget(r.gitTargetName, r.testNs, r.gitProviderName, r.livePath, "main")

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
// This mirrors the Flux receiver webhook wiring (ensureRepoWebhook), but Argo has
// a single /api/webhook endpoint rather than a per-repo Receiver CR.
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

// syncToRevision drives a sync through the Application's `.operation` field —
// the kubectl-only equivalent of `argocd app sync --revision <sha>`. Passing the
// revision explicitly means the sync does not depend on a refresh having landed.
func (r argoBiDirectionalRun) syncToRevision(revision string) {
	GinkgoHelper()
	By(fmt.Sprintf("syncing Argo CD Application %q to revision %s", r.appName, revision[:8]))

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

		finishedAt, _, err := unstructured.NestedString(app.Object, "status", "operationState", "finishedAt")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(finishedAt).NotTo(Equal(previousFinishedAt), "Argo CD has not finished a NEW sync operation yet")

		phase, _, err := unstructured.NestedString(app.Object, "status", "operationState", "phase")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(phase).To(Equal("Succeeded"), "Argo CD sync operation did not succeed")

		synced, _, err := unstructured.NestedString(app.Object, "status", "operationState", "syncResult", "revision")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(synced).To(Equal(revision), "Argo CD synced a different revision")

		// A sync that applies nothing still reports Succeeded. Guard against the
		// empty-path failure mode: if the reverser pruned the manifest before Argo
		// read it, `resources` is empty and the spec would otherwise sail past.
		resources, found, err := unstructured.NestedSlice(
			app.Object, "status", "operationState", "syncResult", "resources")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue(), "Argo CD sync produced no syncResult.resources")
		g.Expect(resources).NotTo(BeEmpty(), "Argo CD synced zero resources — is the source path empty?")
	}, argoSyncTimeout, biPollInterval).Should(Succeed())
}

// operationFinishedAt returns "" when the Application has never run an operation.
func (r argoBiDirectionalRun) operationFinishedAt() string {
	GinkgoHelper()
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

func (r argoBiDirectionalRun) syncStatus() string {
	GinkgoHelper()
	app, err := r.appObject()
	Expect(err).NotTo(HaveOccurred())

	status, _, err := unstructured.NestedString(app.Object, "status", "sync", "status")
	Expect(err).NotTo(HaveOccurred())
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

func (r argoBiDirectionalRun) orderScoopFlavor(name string) (string, error) {
	order, err := r.liveOrder(name)
	if err != nil {
		return "", err
	}
	scoops, found, err := unstructured.NestedSlice(order.Object, "spec", "scoops")
	if err != nil {
		return "", err
	}
	if !found || len(scoops) == 0 {
		return "", fmt.Errorf("IceCreamOrder %q has no scoops", name)
	}
	first, ok := scoops[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("IceCreamOrder %q has a malformed scoop", name)
	}
	flavor, ok := first["flavor"].(string)
	if !ok {
		return "", fmt.Errorf("IceCreamOrder %q scoop has no flavor", name)
	}
	return flavor, nil
}

func (r argoBiDirectionalRun) waitForOrderContainer(name, container string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		value, err := r.orderContainer(name)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(value).To(Equal(container))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r argoBiDirectionalRun) patchOrderContainer(name, container string) {
	GinkgoHelper()
	_, err := kubectlRunInNamespace(r.testNs, "patch", iceCreamCRDName(crdGroupArgoBiDirectional), name,
		"--type=merge", "--patch", fmt.Sprintf(`{"spec":{"container":%q}}`, container))
	Expect(err).NotTo(HaveOccurred(), "failed to patch IceCreamOrder through the API")
}

func (r argoBiDirectionalRun) patchOrderScoop(name, flavor string, quantity int) {
	GinkgoHelper()
	_, err := kubectlRunInNamespace(r.testNs, "patch", iceCreamCRDName(crdGroupArgoBiDirectional), name,
		"--type=merge", "--patch",
		fmt.Sprintf(`{"spec":{"scoops":[{"flavor":%q,"quantity":%d}]}}`, flavor, quantity))
	Expect(err).NotTo(HaveOccurred(), "failed to patch IceCreamOrder scoops through the API")
}

func (r argoBiDirectionalRun) liveOrderPath(name string) string {
	return r.repoPath(r.livePath, iceCreamInstancePath(crdGroupArgoBiDirectional, r.testNs, name))
}

func (r argoBiDirectionalRun) writeLiveOrder(order iceCreamOrderFile) {
	GinkgoHelper()
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
