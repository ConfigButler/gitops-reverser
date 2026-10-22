// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gmeasure"
)

// How long the bi-directional loop takes, as numbers.
//
// The other bi-directional specs prove WHAT happens and only bound how long it may take. These
// time it: each sample starts at the stimulus (an API edit, or a Git-side push landing on the
// server) and stops at the effect an operator would look for, and every duration is recorded in
// a gmeasure experiment that Ginkgo prints as a table (N, min, median, mean, stddev, max) and
// writes into the JSON report.
//
// Each duration also has a budget, and a sample over it fails the spec. The budgets are loose on
// purpose: they catch a path that silently fell back to a periodic tick (minutes), not a slow CI
// runner (seconds). The table is the measurement; the budget is the guarantee.
//
// The folder is SHARED, which is the case a trigger exists for: GitOps Reverser mirrors a
// labeled ConfigMap into the folder the applier applies. The WatchRule selects only that label,
// so the namespace's kube-root-ca.crt never reaches the folder and the applier applies exactly one
// object.
//
// Resolution: the probes poll every timingPoll and each probe is a kubectl or git round trip, so
// a sample reads about a second long at worst. E2E_TIMING_SAMPLES sets the sample count.

const (
	timingPoll = 500 * time.Millisecond

	// An API edit is on the remote branch. The e2e commit window is 0s, so this is the watch
	// event, the commit and the push.
	timingCommitBudget = 45 * time.Second
	// A Git-side push the receiver hears of has reached the live object, through the applier.
	timingReceiverBudget = 90 * time.Second
	// An apply the trigger started has finished, and the trigger holds its applier again.
	timingHeldBudget = 60 * time.Second

	timingDefaultSamples = 3
	timingLabelKey       = "e2e.configbutler.ai/timing"
	timingConfigMap      = "bi-timing"
)

// Measurement names, shared by both appliers so the two tables read side by side.
const (
	measureAPIToCommit      = "API edit → commit on the branch"
	measureCreateToCommit   = "API create → commit on the branch"
	measureCreateToAdopted  = "API create → adopted by the applier"
	measurePushToLive       = "Git-side push → live object (receiver)"
	measurePushToSyncDone   = "Git-side push → Argo CD sync finished (receiver)"
	measurePushToFluxHeld   = "Git-side push → Flux applied it and is held again (receiver)"
	timingSampleValueFormat = "%s-%d"
)

// timingSamples is how many samples each measurement takes.
func timingSamples() int {
	if raw := strings.TrimSpace(os.Getenv("E2E_TIMING_SAMPLES")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n > 0 {
			return n
		}
	}
	return timingDefaultSamples
}

// timedUntil polls probe until it passes, at most budget after start, and records the time from
// start to the first passing probe under name.
func timedUntil(
	experiment *gmeasure.Experiment, name string, start time.Time, budget time.Duration, probe func(g Gomega),
) {
	GinkgoHelper()
	remaining := budget - time.Since(start)
	Expect(remaining).To(BeNumerically(">", 0), "%s: the budget of %s was spent before the probe began", name, budget)
	Eventually(probe, remaining, timingPoll).Should(Succeed(), "%s: over its budget of %s", name, budget)
	experiment.RecordDuration(name, time.Since(start), gmeasure.Precision(100*time.Millisecond))
}

// timingPipeline is one GitTarget mirroring the labeled ConfigMap into the folder an applier
// applies.
type timingPipeline struct {
	testNs       string
	repo         *RepoArtifacts
	providerName string
	targetName   string
	ruleName     string
	gitPath      string
	receiverName string
	hookID       int64
}

func newTimingPipeline(testNs, prefix string, repo *RepoArtifacts) *timingPipeline {
	return &timingPipeline{
		testNs:       testNs,
		repo:         repo,
		providerName: prefix + "-provider",
		targetName:   prefix + "-target",
		ruleName:     prefix + "-rule",
		gitPath:      "timing/" + prefix,
		receiverName: prefix + "-receiver",
	}
}

// filePath is where the GitTarget writes the ConfigMap, relative to the repository root.
func (p *timingPipeline) filePath() string {
	return fmt.Sprintf("%s/%s/configmaps/%s.yaml", p.gitPath, p.testNs, timingConfigMap)
}

// start brings up GitProvider (with a receiver Gitea pushes to), GitTarget and WatchRule, creates
// the ConfigMap, and waits for its first commit, so the folder exists before an applier names it.
func (p *timingPipeline) start() {
	GinkgoHelper()
	_, err := kubectlRunInNamespace(p.testNs, "apply", "-f", p.repo.SecretsYAML)
	Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to the test namespace")
	applySOPSAgeKeyToNamespace(p.testNs)
	configureRepoOriginWithCredentials(p.repo, p.testNs)

	createGitProviderWithURLInNamespace(p.providerName, p.testNs, p.repo.GitSecretHTTP, p.repo.RepoURLHTTP)
	verifyResourceStatus("gitprovider", p.providerName, p.testNs, "True", "Succeeded", "")
	p.hookID = registerOperatorReceiver(p.testNs, p.providerName, p.receiverName, p.repo.RepoName)

	createGitTarget(p.targetName, p.testNs, p.providerName, p.gitPath, "main")
	_, err = kubectlRunWithStdin(p.testNs, fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  rules:
  - resources: ["configmaps"]
    objectSelector:
      matchLabels:
        %s: "true"
`, p.ruleName, p.testNs, p.targetName, timingLabelKey), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply the WatchRule")
	verifyResourceStatus("gittarget", p.targetName, p.testNs, "True", "Succeeded", "")
	waitForStreamsRunning(p.targetName, p.testNs)

	_, err = kubectlRunWithStdin(p.testNs, fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
  labels:
    %s: "true"
data:
  value: seed
`, timingConfigMap, p.testNs, timingLabelKey), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to create the timed ConfigMap")
	Eventually(func(g Gomega) {
		g.Expect(p.committedValue(g)).To(Equal("seed"))
	}, timingCommitBudget, timingPoll).Should(Succeed(), "the ConfigMap must be in Git before an applier applies it")
}

func (p *timingPipeline) cleanup() {
	deleteRepoHook(p.repo.RepoName, p.hookID)
	cleanupWatchRule(p.ruleName, p.testNs)
	cleanupGitTarget(p.targetName, p.testNs)
	cleanupNamespacedResource(p.testNs, "gitprovider", p.providerName)
	cleanupNamespacedResource(p.testNs, "gitreceiver", p.receiverName)
	cleanupNamespacedResource(p.testNs, "secret", p.receiverName)
}

var timingValueLine = regexp.MustCompile(`(?m)^([ \t]+value:[ \t]*).*$`)

// committedFile is the ConfigMap's file on the remote branch.
func (p *timingPipeline) committedFile(g Gomega) string {
	_, err := gitRun(p.repo.CheckoutDir, "fetch", "origin", "main")
	g.Expect(err).NotTo(HaveOccurred())
	out, err := gitRun(p.repo.CheckoutDir, "show", "origin/main:"+p.filePath())
	g.Expect(err).NotTo(HaveOccurred(), "the ConfigMap is not at %s on main", p.filePath())
	return out
}

// committedValue is data.value of the ConfigMap as the remote branch holds it.
func (p *timingPipeline) committedValue(g Gomega) string {
	match := timingValueLine.FindStringSubmatch(p.committedFile(g))
	g.Expect(match).NotTo(BeNil(), "the committed ConfigMap has no data.value")
	return strings.TrimSpace(strings.TrimPrefix(match[0], match[1]))
}

// liveValue is data.value of the live ConfigMap.
func (p *timingPipeline) liveValue(g Gomega) string {
	out, err := kubectlRunInNamespace(p.testNs, "get", "configmap", timingConfigMap, "-o", "jsonpath={.data.value}")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// editThroughAPI sets data.value through the Kubernetes API and returns when the request returned.
func (p *timingPipeline) editThroughAPI(value string) time.Time {
	GinkgoHelper()
	start := time.Now()
	_, err := kubectlRunInNamespace(p.testNs, "patch", "configmap", timingConfigMap, "--type=merge", "-p",
		fmt.Sprintf(`{"data":{"value":%q}}`, value))
	Expect(err).NotTo(HaveOccurred(), "failed to edit the ConfigMap through the API")
	return start
}

// pushFromGitSide rewrites data.value in the committed file, byte for byte otherwise, so the
// applied object mirrors back as no change, and pushes it as another writer would. It returns the
// commit and the moment the push returned, which is when the commit is on the server.
func (p *timingPipeline) pushFromGitSide(value string) (string, time.Time) {
	GinkgoHelper()
	content := timingValueLine.ReplaceAllString(p.committedFile(Default), "${1}"+value)
	head := pushFileFromOutside(p.repo, p.filePath(), content, "e2e: a Git-side timing change to "+value)
	return head, time.Now()
}

// timingSettle outlasts the apply window's grace after an apply finishes (docs/design/apply-trigger.md),
// the time the applier's own write to the object can still be on its way back through the mirror.
const timingSettle = 15 * time.Second

// settled holds, for timingSettle, that the branch is still at head and the live object still
// holds value: the apply mirrored back as no change. A sample starts only once the previous one
// has settled, so each times one change through a quiet loop.
//
// It is not only pacing. Pushing again while the previous apply's own write is still pending is
// a race this spec found with Argo CD: that write, replayed onto the newer push, reverted it. Here
// it would show as a commit after head.
func (p *timingPipeline) settled(head, value string) {
	GinkgoHelper()
	Consistently(func(g Gomega) {
		g.Expect(p.remoteBranchHead(g)).To(Equal(head), "the apply must mirror back as no change")
		g.Expect(p.liveValue(g)).To(Equal(value))
	}, timingSettle, time.Second).Should(Succeed())
}

// remoteBranchHead is where main is on the server.
func (p *timingPipeline) remoteBranchHead(g Gomega) string {
	_, err := gitRun(p.repo.CheckoutDir, "fetch", "origin", "main")
	g.Expect(err).NotTo(HaveOccurred())
	out, err := gitRun(p.repo.CheckoutDir, "rev-parse", "origin/main")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

var _ = Describe("Bi Directional timing (Flux)", Label("bi-directional", "flux", "timing"), Ordered,
	SpecPriority(10), func() {
		var (
			testNs, fluxName string
			pipeline         *timingPipeline
		)

		BeforeAll(func() {
			skipUnlessBiDirectionalEnabled()
			allowCrossNamespaceAppliers()
			testNs = testNamespaceFor("bi-timing-flux")
			_, _ = kubectlRun("create", "namespace", testNs)
			fluxName = "bi-timing-" + testNs

			repo := SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-bi-timing-flux-%d", GinkgoRandomSeed()))
			pipeline = newTimingPipeline(testNs, "flux-timing", repo)
			pipeline.start()

			By("letting Flux apply the shared folder once, then naming a trigger")
			applyFluxApplier(testNs, repo, fluxName, fluxName+"-git", "main", "./"+pipeline.gitPath)
			createFluxTrigger(testNs, fluxName, fluxName)
			nameApplyTrigger(testNs, pipeline.targetName, "FluxTrigger", fluxName, "")
			verifyResourceCondition("gittarget", pipeline.targetName, testNs,
				"ApplyTriggerReady", "True", "Succeeded", "")
			verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Held", "True", "Suspended", "")
		})

		AfterAll(func() {
			if !biDirectionalEnabled() {
				return
			}
			if pipeline != nil {
				// nil when BeforeAll failed before building it.
				pipeline.cleanup()
			}
			cleanupNamespacedResource(testNs, fluxTriggerResource, fluxName)
			cleanupNamespacedResource("flux-system", "kustomization", fluxName)
			cleanupNamespacedResource("flux-system", "gitrepository", fluxName)
			cleanupNamespacedResource("flux-system", "secret", fluxName+"-git")
			cleanupNamespace(testNs)
		})

		// heldAgain passes once the trigger reports the operation for commit Succeeded, with the
		// Kustomization suspended again.

		heldAgain := func(g Gomega, commit string) {
			operation := readTriggerOperation(g, testNs, fluxTriggerResource, fluxName)
			g.Expect(operation.Commit).To(Equal(commit))
			g.Expect(operation.Phase).To(Equal("Succeeded"))
			g.Expect(kustomizationSuspended(g, fluxName)).To(BeTrue())
		}

		// The API side: an edit is mirrored, and asks Flux for no apply; a create is mirrored, and
		// adopted, so Flux owns it and prunes it once Git deletes it.
		It("times an API edit to its commit, and an API create to its adoption", func() {
			apiTimings(fluxEchoApplier(testNs, fluxName, pipeline.repo), pipeline, "Flux")
		})

		// The Git side: another writer pushes, Gitea tells the receiver, and the trigger asks Flux to
		// apply that commit. A ForeignCommit goes out after a flush.
		It("times a Git-side push through the receiver to the live object", func() {
			experiment := gmeasure.NewExperiment("Flux: Git-side push")
			AddReportEntry(experiment.Name, experiment)

			for i := range timingSamples() {
				value := fmt.Sprintf(timingSampleValueFormat, "git", i)
				By("sample " + strconv.Itoa(i) + ": pushing a Git-side change")
				head, pushed := pipeline.pushFromGitSide(value)

				timedUntil(experiment, measurePushToLive, pushed, timingReceiverBudget, func(g Gomega) {
					g.Expect(pipeline.liveValue(g)).To(Equal(value))
				})
				timedUntil(experiment, measurePushToFluxHeld, pushed, timingReceiverBudget+timingHeldBudget,
					func(g Gomega) {
						g.Expect(fluxLastAppliedRevision(g, fluxName)).To(ContainSubstring(head))
						heldAgain(g, head)
					})
				pipeline.settled(head, value)
			}
		})
	})

var _ = Describe("Bi Directional timing (Argo CD)", Label("bi-directional", "argocd", "timing"), Ordered,
	SpecPriority(10), func() {
		var (
			testNs   string
			pipeline *timingPipeline
			run      argoBiDirectionalRun
		)

		BeforeAll(func() {
			skipUnlessBiDirectionalEnabled()
			requireArgoCDInstalled()
			allowCrossNamespaceAppliers()
			testNs = testNamespaceFor("bi-timing-argo")
			_, _ = kubectlRun("create", "namespace", testNs)

			repo := SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-bi-timing-argo-%d", GinkgoRandomSeed()))
			pipeline = newTimingPipeline(testNs, "argo-timing", repo)
			run = newArgoBiDirectionalRun(testNs, repo, "argo-timing")
			pipeline.start()

			By("registering the repository with Argo CD and naming the Application, automated sync off")
			run.applyArgoRepoSecret()
			cfg := run.baseAppConfig()
			cfg.Path = pipeline.gitPath
			run.applyArgoApp(cfg)
			createArgoCDTrigger(testNs, run.appName, run.argoNs, run.appName)
			nameApplyTrigger(testNs, pipeline.targetName, "ArgoCDTrigger", run.appName, "")
			verifyResourceCondition("gittarget", pipeline.targetName, testNs,
				"ApplyTriggerReady", "True", "Succeeded", "")
		})

		AfterAll(func() {
			if !biDirectionalEnabled() {
				return
			}
			cleanupNamespacedResource(run.argoNs, "application.argoproj.io", run.appName)
			cleanupNamespacedResource(run.argoNs, "secret", run.repoSecretName)
			cleanupNamespacedResource(testNs, argoCDTriggerResource, run.appName)
			if pipeline != nil {
				// nil when BeforeAll failed before building it.
				pipeline.cleanup()
			}
			cleanupNamespace(testNs)
		})

		// The API side, as for Flux: an edit asks for no sync, and a create is adopted by a CatchUp sync.
		It("times an API edit to its commit, and an API create to its adoption", func() {
			apiTimings(argoEchoApplier(run), pipeline, "Argo CD")
		})

		// The Git side is a ForeignCommit sync pinned to the pushed commit.
		It("times a Git-side push through the receiver to the live object", func() {
			experiment := gmeasure.NewExperiment("Argo CD: Git-side push")
			AddReportEntry(experiment.Name, experiment)

			for i := range timingSamples() {
				value := fmt.Sprintf(timingSampleValueFormat, "git", i)
				By("sample " + strconv.Itoa(i) + ": pushing a Git-side change")
				head, pushed := pipeline.pushFromGitSide(value)

				timedUntil(experiment, measurePushToLive, pushed, timingReceiverBudget, func(g Gomega) {
					g.Expect(pipeline.liveValue(g)).To(Equal(value))
				})
				timedUntil(experiment, measurePushToSyncDone, pushed, timingReceiverBudget+timingHeldBudget,
					func(g Gomega) {
						operation := run.triggerOperation(g)
						g.Expect(operation.Reason).To(Equal("ForeignCommit"))
						g.Expect(operation.Commit).To(Equal(head))
						g.Expect(operation.Phase).To(Equal("Succeeded"))
					})
				pipeline.settled(head, value)
			}
		})
	})

// apiTimings times the API side for an applier: an edit to its commit, which asks for no apply, and a
// create to its commit and to its adoption, the CatchUp that makes the applier own it.
func apiTimings(applier echoApplier, pipeline *timingPipeline, name string) {
	GinkgoHelper()
	r := &echoRun{timingPipeline: pipeline, applier: applier}
	experiment := gmeasure.NewExperiment(name + ": API round trip")
	AddReportEntry(experiment.Name, experiment)
	experiment.RecordNote("commit window 0s")

	for i := range timingSamples() {
		value := fmt.Sprintf(timingSampleValueFormat, "api", i)
		By("sample " + strconv.Itoa(i) + ": editing the ConfigMap through the API")
		start := pipeline.editThroughAPI(value)
		timedUntil(experiment, measureAPIToCommit, start, timingCommitBudget, func(g Gomega) {
			g.Expect(pipeline.committedValue(g)).To(Equal(value))
		})

		created := fmt.Sprintf("timing-created-%d", i)
		By("sample " + strconv.Itoa(i) + ": creating a ConfigMap through the API")
		start = time.Now()
		r.createThroughAPI(created, value)
		timedUntil(experiment, measureCreateToCommit, start, timingCommitBudget, func(g Gomega) {
			committed, inGit := r.committedValue(g, created)
			g.Expect(inGit).To(BeTrue())
			g.Expect(committed).To(Equal(value))
		})
		timedUntil(experiment, measureCreateToAdopted, start, timingCommitBudget+timingReceiverBudget,
			func(g Gomega) {
				g.Expect(applier.owns(g, pipeline.testNs, created)).To(BeTrue())
				g.Expect(applier.operation(g).Phase).To(Equal("Succeeded"))
			})
		Expect(pipeline.liveValue(Default)).To(Equal(value), "the adoption must not revert the edit")
	}
}
