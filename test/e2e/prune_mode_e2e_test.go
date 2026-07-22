// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This spec is the end-to-end proof for GitTarget.spec.prune.mode (see
// docs/design/watchrule-source-namespace/pr5-gittarget-deletion-safety.md). It exercises the two
// deletion paths SEPARATELY, because the whole design rests on them being independently
// controlled — `onEvent`, the effective default, differs from `always` on exactly one of them.
//
// Every assertion here is paired with a BARRIER: a negative claim ("the file is still there") is
// worthless on its own, since it also passes when the pipeline is simply asleep. So each retention
// assertion is made only after a co-resident GitTarget, fed by the SAME cluster event or the SAME
// resync trigger, has been observed to act. That is what makes "it did not delete" mean "it
// decided not to delete" rather than "nothing happened yet".
var _ = Describe("Manager GitTarget prune policy", Label("manager"), Ordered, func() {
	const (
		providerName = "gitprovider-prune"

		defaultTarget = "prune-default-target"
		neverTarget   = "prune-never-target"
		alwaysTarget  = "prune-always-target"

		defaultPath = "e2e/prune-default"
		neverPath   = "e2e/prune-never"
		alwaysPath  = "e2e/prune-always"

		defaultRule = "prune-default-rule"
		neverRule   = "prune-never-rule"
		alwaysRule  = "prune-always-rule"

		// Seeded by the sweep spec and RETAINED by the default target, which is what the two specs
		// after it observe: first on status, then being swept once the policy is widened. Shared
		// here rather than repeated, so the coupling between those specs is visible.
		orphanName = "prune-orphan"
	)

	var (
		testNs    string
		pruneRepo *RepoArtifacts
	)

	BeforeAll(func() {
		By("creating the prune-policy test namespace")
		testNs = testNamespaceFor("manager-prune")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up the Gitea repo and credentials")
		pruneRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-manager-prune-%d", GinkgoRandomSeed()),
		)
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", pruneRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")

		createReadyGitProvider(providerName, testNs, pruneRepo.GitSecretHTTP, pruneRepo.RepoURLHTTP)

		By("creating three GitTargets in one repo, one per prune policy")
		// The default target declares NO prune block at all — the shape an existing cluster
		// holds after upgrading into this release. Its behaviour must come from
		// EffectivePruneMode, not from a CRD default it never received.
		applyPruneGitTarget(defaultTarget, testNs, providerName, defaultPath, "")
		applyPruneGitTarget(neverTarget, testNs, providerName, neverPath, "Never")
		applyPruneGitTarget(alwaysTarget, testNs, providerName, alwaysPath, "Always")
		for _, name := range []string{defaultTarget, neverTarget, alwaysTarget} {
			verifyResourceCondition("gittarget", name, testNs, "Validated", "True", "Succeeded", "")
		}

		By("each target watches ConfigMaps in this namespace")
		applyIsolationWatchRule(defaultRule, testNs, defaultTarget, `"configmaps"`)
		applyIsolationWatchRule(neverRule, testNs, neverTarget, `"configmaps"`)
		applyIsolationWatchRule(alwaysRule, testNs, alwaysTarget, `"configmaps"`)
		for _, name := range []string{defaultRule, neverRule, alwaysRule} {
			verifyResourceStatus("watchrule", name, testNs, "True", "Succeeded", "")
		}

		By("waiting for every target's ConfigMap stream to be live before any event is created")
		for _, name := range []string{defaultTarget, neverTarget, alwaysTarget} {
			waitForStreamsRunning(name, testNs)
		}
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(60 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// The API contract, checked against the live apiserver rather than against the Go types: the
	// schema must default a declared-but-empty prune block, must leave an omitted one absent
	// (which is exactly why EffectivePruneMode exists), and must reject a value outside the enum.
	It("defaults, omits, and validates spec.prune.mode at the API server", func() {
		By("an omitted prune block stays omitted — Kubernetes does not default an absent object")
		Expect(pruneModeOf(defaultTarget, testNs)).To(BeEmpty(),
			"a GitTarget with no prune block must persist without one, so the omitted case is real")

		By("a declared-but-empty prune block is defaulted to OnEvent by the schema")
		declaredEmpty := "prune-defaulted-target"
		Expect(applyRawGitTarget(declaredEmpty, testNs, providerName, "e2e/prune-defaulted", "  prune: {}")).
			To(Succeed(), "a GitTarget declaring an empty prune block must be accepted")
		Expect(pruneModeOf(declaredEmpty, testNs)).To(Equal("OnEvent"),
			"the CRD default must write onEvent into a newly created object")

		By("a mode outside the enum is rejected")
		err := applyRawGitTarget("prune-bogus-target", testNs, providerName, "e2e/prune-bogus",
			"  prune:\n    mode: sometimes")
		Expect(err).To(HaveOccurred(), "an unsupported prune mode must be rejected by the schema")
		Expect(err.Error()).To(ContainSubstring("mode"),
			"the rejection must name the offending field")
	})

	// PATH 1 — the explicit source DELETE. `never` is the only mode that suppresses it, so this
	// is the assertion that distinguishes `never` from the default; a test that only covered the
	// sweep would pass for both.
	It("mirrors an observed DELETE under the default and retains it under never", func() {
		const cmName = "prune-delete-me"

		By("creating a watched ConfigMap and waiting for both targets to mirror it")
		applyIsolationConfigMap(cmName, testNs)
		defaultFile := pruneConfigMapPath(defaultPath, testNs, cmName)
		neverFile := pruneConfigMapPath(neverPath, testNs, cmName)
		waitForPruneFile(pruneRepo, defaultFile, true)
		waitForPruneFile(pruneRepo, neverFile, true)

		By("deleting the ConfigMap from the cluster")
		_, err := kubectlRunInNamespace(testNs, "delete", "configmap", cmName)
		Expect(err).NotTo(HaveOccurred(), "deleting the watched ConfigMap should succeed")

		// The barrier: the default target consumes the same DELETE event from the same watch and
		// removes its copy. Once that is observed, the event has demonstrably reached the writer,
		// so the never target's surviving copy is a decision rather than a pending write.
		By("the default target (effective mode onEvent) removes its copy")
		waitForPruneFile(pruneRepo, defaultFile, false)

		By("the never target keeps its copy, and keeps it")
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, pruneRepo.CheckoutDir)
			_, statErr := os.Stat(filepath.Join(pruneRepo.CheckoutDir, neverFile))
			g.Expect(statErr).NotTo(HaveOccurred(),
				"prune.mode: Never must not mirror a source DELETE")
		}, 15*time.Second, 3*time.Second).Should(Succeed())
	})

	// PATH 2 — the inferred mark-and-sweep. This is the path PR 5 exists for: a document in Git
	// that the cluster has no counterpart for is a DELETION ONLY IF the desired snapshot is
	// trusted, and a narrowed snapshot is exactly what a scope mistake produces.
	//
	// The orphan is seeded by pushing straight into the repo, because that is the only way to
	// manufacture "Git has a managed document the cluster does not" without also making the
	// cluster emit an event about it.
	It("sweeps an orphaned document only when prune.mode is Always", func() {
		By("seeding an orphaned ConfigMap manifest into both the always and default folders")
		alwaysOrphan := pruneConfigMapPath(alwaysPath, testNs, orphanName)
		defaultOrphan := pruneConfigMapPath(defaultPath, testNs, orphanName)
		seedOrphanManifests(pruneRepo, testNs, map[string]string{
			alwaysOrphan:  orphanConfigMapYAML(orphanName, testNs),
			defaultOrphan: orphanConfigMapYAML(orphanName, testNs),
		})

		// Toggling ConfigMaps OFF and back ON is what makes this deterministic: it tears the
		// configmaps stream down and re-establishes it, and a re-established stream replays and
		// issues a resync scoped to exactly (configmaps, this namespace) — the scope the seeded
		// orphan lives in. Merely ADDING an unrelated type would churn the rule without
		// guaranteeing the configmaps stream restarts, and the sweep would never be attempted.
		By("toggling ConfigMaps off and back on to force a scoped replay resync")
		applyIsolationWatchRule(alwaysRule, testNs, alwaysTarget, `"services"`)
		applyIsolationWatchRule(defaultRule, testNs, defaultTarget, `"services"`)
		applyIsolationWatchRule(alwaysRule, testNs, alwaysTarget, `"configmaps"`)
		applyIsolationWatchRule(defaultRule, testNs, defaultTarget, `"configmaps"`)
		waitForStreamsRunning(alwaysTarget, testNs)
		waitForStreamsRunning(defaultTarget, testNs)

		// The barrier: the always target's resync reaches the same folder with the same desired
		// snapshot as the default target's. Observing its sweep proves a resync ran and that the
		// seeded document was in its scope — without which "still present" would prove nothing.
		By("the always target sweeps the orphan")
		waitForPruneFile(pruneRepo, alwaysOrphan, false)

		By("the default target (effective mode onEvent) keeps it")
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, pruneRepo.CheckoutDir)
			_, statErr := os.Stat(filepath.Join(pruneRepo.CheckoutDir, defaultOrphan))
			g.Expect(statErr).NotTo(HaveOccurred(),
				"the default prune mode must never infer a deletion from a desired snapshot")
		}, 15*time.Second, 3*time.Second).Should(Succeed())

		By("and the default target is still mirroring — retention is not a stalled pipeline")
		const proofName = "prune-still-live"
		applyIsolationConfigMap(proofName, testNs)
		waitForPruneFile(pruneRepo, pruneConfigMapPath(defaultPath, testNs, proofName), true)
	})

	// A suppressed sweep leaves no action, no commit, and no stat — deliberately, so a retention is
	// indistinguishable from the event never arriving. status.retention is the one place it becomes
	// visible, and this asserts BOTH of its states from the same seeded orphan: the default target
	// reports what it kept, while the co-resident always target reports zero. Zero is the converged
	// signal, and it only means anything if it is published as actively as a non-zero count.
	It("reports retained documents, and convergence, on GitTarget status", func() {
		By("the default target reports the documents its policy kept")
		Eventually(func(g Gomega) {
			g.Expect(retainedDocumentsOf(g, defaultTarget, testNs)).To(BeNumerically(">", 0),
				"the orphan the previous spec retained must be visible on status")
			g.Expect(retentionModeOf(g, defaultTarget, testNs)).To(Equal("OnEvent"),
				"status must report the EFFECTIVE mode; this target stores no prune block at all")
		}).Should(Succeed())

		By("the always target reports zero — it converged rather than never having reported")
		Eventually(func(g Gomega) {
			g.Expect(retainedDocumentsOf(g, alwaysTarget, testNs)).To(Equal(0))
			g.Expect(retentionModeOf(g, alwaysTarget, testNs)).To(Equal("Always"))
		}).Should(Succeed())

		By("no condition went False for a retention — it is the configured outcome, not a fault")
		verifyResourceCondition("gittarget", defaultTarget, testNs, "Ready", "True", "", "")
	})

	// The migration instruction this release ships with is "declare always to keep the old
	// behaviour". That is only true if the edit itself converges the mirror: the watch specs
	// describe what is watched, not what may be deleted, so a prune edit changes none of them, and
	// a reconnect resumes from its cursor rather than replaying. Without the widening being its own
	// trigger, a quiet target could sit under always indefinitely and never sweep.
	//
	// Deliberately NO WatchRule change here — that is the whole point. The previous sweep spec had
	// to toggle the rule to force a resync; if this spec ever needs the same crutch, the fix has
	// regressed.
	It("converges an existing orphan when prune.mode is widened, without touching the WatchRule", func() {
		const lateOrphanName = "prune-late-orphan"

		By("seeding a second orphan that no cluster event will ever mention")
		lateOrphan := pruneConfigMapPath(defaultPath, testNs, lateOrphanName)
		seedOrphanManifests(pruneRepo, testNs, map[string]string{
			lateOrphan: orphanConfigMapYAML(lateOrphanName, testNs),
		})
		waitForPruneFile(pruneRepo, lateOrphan, true)

		By("widening the default target's policy to always — the only action this spec takes")
		_, err := kubectlRunInNamespace(testNs, "patch", "gittarget", defaultTarget,
			"--type=merge", "-p", `{"spec":{"prune":{"mode":"Always"}}}`)
		Expect(err).NotTo(HaveOccurred(), "spec.prune is mutable and the patch must be accepted")

		By("the newly authorized sweep removes both retained orphans")
		waitForPruneFile(pruneRepo, lateOrphan, false)
		waitForPruneFile(pruneRepo, pruneConfigMapPath(defaultPath, testNs, orphanName), false)

		By("and status follows the sweep back to a converged zero under the new mode")
		waitForStreamsRunning(defaultTarget, testNs)
		Eventually(func(g Gomega) {
			g.Expect(retainedDocumentsOf(g, defaultTarget, testNs)).To(Equal(0),
				"a resync that retains nothing must drive the count back to zero, not leave it stale")
			g.Expect(retentionModeOf(g, defaultTarget, testNs)).To(Equal("Always"))
		}).Should(Succeed())

		By("the target still mirrors live events after the forced replay")
		const proofName = "prune-post-widen"
		applyIsolationConfigMap(proofName, testNs)
		waitForPruneFile(pruneRepo, pruneConfigMapPath(defaultPath, testNs, proofName), true)
	})
})

// retainedDocumentsOf reads status.retention.retainedDocuments. An ABSENT retention block fails the
// read rather than reporting zero: the two mean different things (nothing reported yet vs. a resync
// found nothing), and collapsing them here would let a spec pass before any resync had run.
func retainedDocumentsOf(g Gomega, name, namespace string) int {
	out, err := kubectlRunInNamespace(namespace, "get", "gittarget", name,
		"-o", "jsonpath={.status.retention.retainedDocuments}")
	g.Expect(err).NotTo(HaveOccurred(), "failed to read status.retention of %q", name)
	value := strings.TrimSpace(out)
	g.Expect(value).NotTo(BeEmpty(), "%q has not reported a retention roll-up yet", name)
	count, convErr := strconv.Atoi(value)
	g.Expect(convErr).NotTo(HaveOccurred(), "retainedDocuments %q is not a number", value)
	return count
}

// retentionModeOf reads status.retention.mode — the effective prune mode the count was produced
// under, which for a legacy GitTarget is the only place that mode is visible at all.
func retentionModeOf(g Gomega, name, namespace string) string {
	out, err := kubectlRunInNamespace(namespace, "get", "gittarget", name,
		"-o", "jsonpath={.status.retention.mode}")
	g.Expect(err).NotTo(HaveOccurred(), "failed to read status.retention.mode of %q", name)
	return strings.TrimSpace(out)
}

// applyPruneGitTarget creates a GitTarget with the given prune mode. An empty mode omits the
// prune block entirely — the legacy shape, which must resolve to OnEvent without being edited.
func applyPruneGitTarget(name, namespace, providerName, targetPath, mode string) {
	GinkgoHelper()
	data := struct {
		Name         string
		Namespace    string
		ProviderName string
		Branch       string
		Path         string
		PruneMode    string
	}{
		Name:         name,
		Namespace:    namespace,
		ProviderName: providerName,
		Branch:       "main",
		Path:         targetPath,
		PruneMode:    mode,
	}
	Expect(applyFromTemplate("test/e2e/templates/manager/gittarget-prune.tmpl", data, namespace)).
		To(Succeed(), "failed to apply GitTarget %q with prune mode %q", name, mode)
}

// applyRawGitTarget applies a GitTarget whose prune block is supplied verbatim, so a spec can
// send the API server a body the typed template cannot express (an empty object, or an invalid
// enum value). It returns the apply error rather than asserting, since rejection is the point.
func applyRawGitTarget(name, namespace, providerName, targetPath, pruneBlock string) error {
	GinkgoHelper()
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
%s
`, name, namespace, providerName, targetPath, pruneBlock)
	out, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// pruneModeOf reads a GitTarget's stored spec.prune.mode, empty when the field is absent.
func pruneModeOf(name, namespace string) string {
	GinkgoHelper()
	out, err := kubectlRunInNamespace(namespace, "get", "gittarget", name,
		"-o", "jsonpath={.spec.prune.mode}")
	Expect(err).NotTo(HaveOccurred(), "failed to read spec.prune.mode of %q", name)
	return strings.TrimSpace(out)
}

// pruneConfigMapPath is the canonical mirror path for a ConfigMap under a GitTarget folder.
func pruneConfigMapPath(basePath, ns, name string) string {
	return path.Join(basePath, fmt.Sprintf("%s/configmaps/%s.yaml", ns, name))
}

// waitForPruneFile waits until a repo-relative path is present (or absent), pulling fresh state
// on each attempt.
func waitForPruneFile(repo *RepoArtifacts, relPath string, wantPresent bool) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		pullLatestRepoState(g, repo.CheckoutDir)
		_, statErr := os.Stat(filepath.Join(repo.CheckoutDir, relPath))
		if wantPresent {
			g.Expect(statErr).NotTo(HaveOccurred(), "%s should exist", relPath)
			return
		}
		g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "%s should be gone", relPath)
	}).Should(Succeed())
}

// orphanConfigMapYAML renders a ConfigMap manifest for a resource that does NOT exist in the
// cluster — a managed document with no desired counterpart, which is precisely what a
// mark-and-sweep resync classifies as a managed drop.
func orphanConfigMapYAML(name, namespace string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  seeded-by: e2e-prune-spec
`, name, namespace)
}

// seedOrphanManifests commits files straight into the repo's main branch, bypassing the operator.
// It mirrors the push-with-rebase-retry shape the in-place edit spec uses, because the operator
// pushes to the same branch concurrently and a lost race must not fail the spec.
func seedOrphanManifests(repo *RepoArtifacts, namespace string, filesByRelPath map[string]string) {
	GinkgoHelper()

	configureRepoOriginWithCredentials(repo, namespace)
	mustGit := func(args ...string) {
		out, gitErr := gitRun(repo.CheckoutDir, args...)
		Expect(gitErr).NotTo(HaveOccurred(), fmt.Sprintf("git %s: %s", strings.Join(args, " "), out))
	}

	mustGit("fetch", "origin", "main")
	mustGit("checkout", "-B", "main", "origin/main")
	mustGit("reset", "--hard", "origin/main")

	for relPath, body := range filesByRelPath {
		full := filepath.Join(repo.CheckoutDir, relPath)
		Expect(os.MkdirAll(filepath.Dir(full), 0o750)).To(Succeed())
		Expect(os.WriteFile(full, []byte(body), 0o600)).To(Succeed())
		mustGit("add", relPath)
	}
	mustGit("commit", "-m", "e2e: seed an orphaned managed document")
	if _, pushErr := gitRun(repo.CheckoutDir, "push", "origin", "HEAD:main"); pushErr != nil {
		// Lost a race with the operator's own push: rebase onto the new tip and retry.
		mustGit("fetch", "origin", "main")
		mustGit("rebase", "origin/main")
		mustGit("push", "origin", "HEAD:main")
	}
}
