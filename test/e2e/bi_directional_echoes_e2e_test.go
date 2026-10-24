// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The applier echoes, end to end (docs/design/applier-echoes.md, "New end-to-end scenarios"). Each
// scenario runs for Flux and for Argo CD on a folder GitOps Reverser mirrors and the applier
// applies: the timing pipeline's shared folder, where a WatchRule mirrors the labeled ConfigMaps.
// The push race (E1) is bi_directional_push_race_e2e_test.go, the restart (E9) is
// bi_directional_restart_e2e_test.go, and the timing (E13) is bi_directional_timing_e2e_test.go.

const (
	// echoApplyBudget is how long an apply GitOps Reverser asks for may take to show on the cluster.
	echoApplyBudget = 2 * time.Minute
	// echoHold is how long a state must hold to count as one nothing reverts.
	echoHold = 20 * time.Second
)

// echoApplier is what a scenario needs to know about the applier of the folder.
type echoApplier struct {
	name string
	// owns reports whether the applier owns the live ConfigMap: Flux's owner labels name its
	// Kustomization, or Argo CD's tracking annotation names its Application.
	owns func(g Gomega, testNs, configMap string) bool
	// operation is the applier claim's status.operation.
	operation func(g Gomega) claimOperation
	// startOther starts another applier applying folder, which is not the applier claim's, and returns its
	// cleanup.
	startOther func(folder string) func()
}

// fluxEchoApplier is the FluxClaim fluxName of testNs, holding the Kustomization of that name.
func fluxEchoApplier(testNs, fluxName string, repo *RepoArtifacts) echoApplier {
	return echoApplier{
		name: "Flux",
		owns: func(g Gomega, ns, configMap string) bool {
			out, err := kubectlRunInNamespace(ns, "get", "configmap", configMap,
				`-o=jsonpath={.metadata.labels.kustomize\.toolkit\.fluxcd\.io/name}`)
			g.Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(out) == fluxName
		},
		operation: func(g Gomega) claimOperation {
			return readClaimOperation(g, testNs, fluxClaimResource, fluxName)
		},
		startOther: func(folder string) func() {
			other := "other-" + fluxName
			applyFluxApplierWithPrune(testNs, repo, other, other+"-git", "main", "./"+folder, true)
			return func() {
				cleanupNamespacedResource("flux-system", "kustomization", other)
				cleanupNamespacedResource("flux-system", "gitrepository", other)
				cleanupNamespacedResource("flux-system", "secret", other+"-git")
			}
		},
	}
}

// argoEchoApplier is the ArgoCDClaim of run, holding its Application.
func argoEchoApplier(run argoBiDirectionalRun) echoApplier {
	return echoApplier{
		name: "Argo CD",
		owns: func(g Gomega, ns, configMap string) bool {
			out, err := kubectlRunInNamespace(ns, "get", "configmap", configMap,
				`-o=jsonpath={.metadata.annotations.argocd\.argoproj\.io/tracking-id}`)
			g.Expect(err).NotTo(HaveOccurred())
			return strings.HasPrefix(strings.TrimSpace(out), run.appName+":")
		},
		operation: run.claimOperation,
		startOther: func(folder string) func() {
			other := run.baseAppConfig()
			other.Name = "other-" + run.appName
			other.Path = folder
			other.Automated = true // not an applier claim's: it syncs on its own
			run.applyArgoApp(other)
			return func() { cleanupNamespacedResource(run.argoNs, "application.argoproj.io", other.Name) }
		},
	}
}

// echoRun is one GitTarget mirroring the labeled ConfigMaps of testNs into the folder an applier
// applies.
type echoRun struct {
	*timingPipeline

	applier echoApplier
}

// cmFile is where the GitTarget writes the ConfigMap name.
func (r *echoRun) cmFile(name string) string {
	return fmt.Sprintf("%s/%s/configmaps/%s.yaml", r.gitPath, r.testNs, name)
}

// echoConfigMap is a labeled ConfigMap the WatchRule mirrors.
func echoConfigMap(namespace, name, value string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  labels:
    %s: "true"
  name: %s
  namespace: %s
data:
  value: %s
`, timingLabelKey, name, namespace, value)
}

// createThroughAPI creates the ConfigMap name holding value through the Kubernetes API.
func (r *echoRun) createThroughAPI(name, value string) {
	GinkgoHelper()
	_, err := kubectlRunWithStdin(r.testNs, echoConfigMap(r.testNs, name, value), "create", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to create ConfigMap %s through the API", name)
}

// editThroughAPI sets data.value of the ConfigMap name through the Kubernetes API.
func (r *echoRun) editThroughAPI(name, value string) {
	GinkgoHelper()
	_, err := kubectlRunInNamespace(r.testNs, "patch", "configmap", name, "--type=merge", "-p",
		fmt.Sprintf(`{"data":{"value":%q}}`, value))
	Expect(err).NotTo(HaveOccurred(), "failed to edit ConfigMap %s through the API", name)
}

// committedValue is data.value of the ConfigMap name as main holds it, and whether main holds it.
func (r *echoRun) committedValue(g Gomega, name string) (string, bool) {
	_, err := gitRun(r.repo.CheckoutDir, "fetch", "origin", "main")
	g.Expect(err).NotTo(HaveOccurred())
	out, err := gitRun(r.repo.CheckoutDir, "show", "origin/main:"+r.cmFile(name))
	if err != nil {
		return "", false
	}
	match := timingValueLine.FindStringSubmatch(out)
	g.Expect(match).NotTo(BeNil(), "the committed ConfigMap %s has no data.value", name)
	return strings.TrimSpace(strings.TrimPrefix(match[0], match[1])), true
}

// liveValue is data.value of the live ConfigMap name, and whether it exists.
func (r *echoRun) liveValue(g Gomega, name string) (string, bool) {
	out, err := kubectlRunInNamespace(r.testNs, "get", "configmap", name, "--ignore-not-found",
		"-o", "jsonpath={.metadata.name}|{.data.value}")
	g.Expect(err).NotTo(HaveOccurred())
	gotName, value, _ := strings.Cut(strings.TrimSpace(out), "|")
	return value, gotName != ""
}

// pushFromOutside writes the ConfigMap name holding value into the folder and pushes it the way
// another writer would.
func (r *echoRun) pushFromOutside(name, value string) {
	GinkgoHelper()
	pushFileFromOutside(r.repo, r.cmFile(name), echoConfigMap(r.testNs, name, value),
		fmt.Sprintf("e2e: a Git-side change sets %s to %s", name, value))
}

// deleteFromOutside deletes the ConfigMap name's file and pushes the delete the way another writer
// would.
func (r *echoRun) deleteFromOutside(name string) {
	GinkgoHelper()
	removeFileFromOutside(r.repo, r.cmFile(name), "e2e: a Git-side delete of "+name)
}

// forceResync asks the GitTarget to re-anchor its streams, which replays each and resyncs its folder.
func (r *echoRun) forceResync() {
	GinkgoHelper()
	requestGitTargetReconcile(r.targetName, r.testNs)
}

// eventuallyEverywhere waits until main and the cluster both hold value for the ConfigMap name.
func (r *echoRun) eventuallyEverywhere(name, value string, budget time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		committed, inGit := r.committedValue(g, name)
		g.Expect(inGit).To(BeTrue(), "%s is not in Git", name)
		g.Expect(committed).To(Equal(value), "Git")
		live, exists := r.liveValue(g, name)
		g.Expect(exists).To(BeTrue(), "%s is not on the cluster", name)
		g.Expect(live).To(Equal(value), "the cluster")
	}, budget, time.Second).Should(Succeed())
}

// consistentlyEverywhere holds that main and the cluster both keep value for the ConfigMap name.
func (r *echoRun) consistentlyEverywhere(name, value string) {
	GinkgoHelper()
	Consistently(func(g Gomega) {
		committed, _ := r.committedValue(g, name)
		g.Expect(committed).To(Equal(value), "Git must keep %s", value)
		live, _ := r.liveValue(g, name)
		g.Expect(live).To(Equal(value), "the cluster must keep %s", value)
	}, echoHold, 2*time.Second).Should(Succeed())
}

// eventuallyAdopted waits until the applier owns the ConfigMap name, after an operation that
// succeeded.
func (r *echoRun) eventuallyAdopted(name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		g.Expect(r.applier.owns(g, r.testNs, name)).To(BeTrue(), "the applier must own %s", name)
		g.Expect(r.applier.operation(g).Phase).To(Equal("Succeeded"))
	}, echoApplyBudget, 2*time.Second).Should(Succeed())
}

// eventuallyPruned waits until the ConfigMap name is gone from the cluster, and holds that it stays
// gone from Git.
func (r *echoRun) eventuallyPruned(name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		_, exists := r.liveValue(g, name)
		g.Expect(exists).To(BeFalse(), "the applier must prune %s, which it owns", name)
	}, echoApplyBudget, 2*time.Second).Should(Succeed())
	Consistently(func(g Gomega) {
		_, inGit := r.committedValue(g, name)
		g.Expect(inGit).To(BeFalse(), "a resync must not write the pruned %s back", name)
	}, echoHold, 2*time.Second).Should(Succeed())
}

// echoScenarios are the scenarios that run the same for both appliers, against run.
func echoScenarios(run func() *echoRun) {
	It("adopts an object the API created, so a Git-side delete prunes it (E2)", func() {
		r := run()
		r.createThroughAPI("adopt-created", "c1")
		r.eventuallyEverywhere("adopt-created", "c1", timingCommitBudget)
		r.eventuallyAdopted("adopt-created")

		r.deleteFromOutside("adopt-created")
		r.eventuallyPruned("adopt-created")
	})

	It("adopts an object deleted and created again with the same content (E3)", func() {
		r := run()
		r.pushFromOutside("recreated", "r1")
		r.eventuallyEverywhere("recreated", "r1", echoApplyBudget)
		r.eventuallyAdopted("recreated")

		By("deleting it through the API, and creating it again with identical content")
		_, err := kubectlRunInNamespace(r.testNs, "delete", "configmap", "recreated", "--wait=true")
		Expect(err).NotTo(HaveOccurred())
		r.createThroughAPI("recreated", "r1")
		r.eventuallyEverywhere("recreated", "r1", echoApplyBudget)
		r.eventuallyAdopted("recreated")

		r.deleteFromOutside("recreated")
		r.eventuallyPruned("recreated")
	})

	It("keeps an edit made during an apply, in Git at once and on the cluster after the Repair (E5)", func() {
		r := run()
		r.pushFromOutside("during", "d0")
		r.eventuallyEverywhere("during", "d0", echoApplyBudget)

		By("pushing a Git-side change to another object, and editing this one while that apply runs")
		before := r.applier.operation(Default).Token
		r.pushFromOutside("during-other", "o1")
		Eventually(func(g Gomega) {
			g.Expect(r.applier.operation(g).Token).NotTo(Equal(before), "the apply is asked for")
		}, echoApplyBudget, 250*time.Millisecond).Should(Succeed())
		r.editThroughAPI("during", "d-edit")

		r.eventuallyEverywhere("during", "d-edit", echoApplyBudget)
		r.eventuallyEverywhere("during-other", "o1", echoApplyBudget)
		r.consistentlyEverywhere("during", "d-edit")
	})

	It("applies a Git-side push while edits keep arriving, and keeps every edit (E6)", func() {
		r := run()
		r.pushFromOutside("busy", "b0")
		r.eventuallyEverywhere("busy", "b0", echoApplyBudget)

		By("editing an object every second while a Git-side push to another one waits")
		stop := make(chan struct{})
		var wg sync.WaitGroup
		last := "b0"
		wg.Go(func() {
			defer GinkgoRecover()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				case <-time.After(time.Second):
				}
				value := fmt.Sprintf("busy-%d", i)
				r.editThroughAPI("busy", value)
				last = value // read only after wg.Wait
			}
		})
		r.pushFromOutside("busy-push", "p1")
		Eventually(func(g Gomega) {
			live, _ := r.liveValue(g, "busy-push")
			g.Expect(live).To(Equal("p1"), "the push is applied while edits keep arriving")
		}, timingReceiverBudget, time.Second).Should(Succeed())
		close(stop)
		wg.Wait()

		r.eventuallyEverywhere("busy", last, echoApplyBudget)
		r.consistentlyEverywhere("busy", last)
	})

	It("keeps a Git-side push when a resync is forced before its apply finishes (E7)", func() {
		r := run()
		r.pushFromOutside("ahead", "a0")
		r.eventuallyEverywhere("ahead", "a0", echoApplyBudget)

		r.pushFromOutside("ahead", "a1")
		r.forceResync()
		r.eventuallyEverywhere("ahead", "a1", echoApplyBudget)
		r.consistentlyEverywhere("ahead", "a1")
	})

	It("keeps a Git-side addition across a resync under prune.mode Always (E8)", func() {
		r := run()
		_, err := kubectlRunInNamespace(r.testNs, "patch", "gittarget", r.targetName, "--type=merge", "-p",
			`{"spec":{"prune":{"mode":"Always"}}}`)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = kubectlRunInNamespace(r.testNs, "patch", "gittarget", r.targetName, "--type=merge", "-p",
				`{"spec":{"prune":null}}`)
		})

		r.pushFromOutside("added-in-git", "g1")
		r.forceResync()
		r.eventuallyEverywhere("added-in-git", "g1", echoApplyBudget)
		r.consistentlyEverywhere("added-in-git", "g1")
	})

	It("asks for no apply after an API edit, and keeps the later of two quick edits (E10)", func() {
		r := run()
		r.pushFromOutside("quick", "q0")
		r.eventuallyEverywhere("quick", "q0", echoApplyBudget)
		r.eventuallyAdopted("quick")
		before := r.applier.operation(Default)

		r.editThroughAPI("quick", "q1")
		r.editThroughAPI("quick", "q2")
		r.eventuallyEverywhere("quick", "q2", timingCommitBudget)
		Consistently(func(g Gomega) {
			g.Expect(r.applier.operation(g).Token).To(Equal(before.Token), "an API edit asks for no apply")
			live, _ := r.liveValue(g, "quick")
			g.Expect(live).To(Equal("q2"))
		}, echoHold, 2*time.Second).Should(Succeed())
	})

	It("does not mirror or adopt an object another applier owns (E11)", func() {
		r := run()
		folder := "other/" + r.testNs
		head := pushFileFromOutside(r.repo, folder+"/owned-elsewhere.yaml",
			echoConfigMap(r.testNs, "owned-elsewhere", "e1"), "e2e: another applier's folder")
		// The push is a foreign commit to the applier claim's repository too, so it is applied first; what
		// must not follow is an apply of the object the other applier creates.
		var before claimOperation
		Eventually(func(g Gomega) {
			before = r.applier.operation(g)
			g.Expect(before.Commit).To(Equal(head), "the push is applied")
			g.Expect(before.Phase).To(Equal("Succeeded"))
		}, echoApplyBudget, 2*time.Second).Should(Succeed())
		stopOther := r.applier.startOther(folder)
		DeferCleanup(stopOther)

		Eventually(func(g Gomega) {
			live, exists := r.liveValue(g, "owned-elsewhere")
			g.Expect(exists).To(BeTrue(), "the other applier applies it")
			g.Expect(live).To(Equal("e1"))
		}, echoApplyBudget, 2*time.Second).Should(Succeed())
		Consistently(func(g Gomega) {
			_, inGit := r.committedValue(g, "owned-elsewhere")
			g.Expect(inGit).To(BeFalse(), "it is not mirrored into the coordinated folder")
			g.Expect(r.applier.operation(g).Token).To(Equal(before.Token), "nor adopted")
		}, echoHold, 2*time.Second).Should(Succeed())
	})

	It("resolves a save made across a Git-side change from the push (E12)", func() {
		r := run()
		r.pushFromOutside("saved", "s0")
		r.eventuallyEverywhere("saved", "s0", echoApplyBudget)

		By("saving an API edit with a CommitRequest while a Git-side push changes the same object")
		name := applyCommitRequestWithGenerateName(r.testNs, "echo-save-", r.targetName, "e2e: save the edit")
		r.editThroughAPI("saved", "s-save")
		r.pushFromOutside("saved", "s-git")

		Eventually(func(g Gomega) {
			g.Expect(commitRequestCondition(g, r.testNs, name, "Ready")).To(Equal("True"))
			g.Expect(commitRequestField(g, r.testNs, name, `{.status.conditions[?(@.type=="Ready")].reason}`)).
				To(BeElementOf(commitRequestReasonCommitted, "AlreadyPresent"))
		}, echoApplyBudget, 2*time.Second).Should(Succeed())
		r.eventuallyEverywhere("saved", "s-save", echoApplyBudget)
		r.consistentlyEverywhere("saved", "s-save")
	})
}

// removeFileFromOutside deletes one file and pushes the delete to main the way another writer
// would. It retries, because GitOps Reverser may move main in between.
func removeFileFromOutside(repo *RepoArtifacts, path, message string) {
	GinkgoHelper()
	Eventually(func() error {
		for _, args := range [][]string{
			{"fetch", "origin", "main"}, {"checkout", "-B", "main", "origin/main"}, {"reset", "--hard", "origin/main"},
		} {
			if out, err := gitRun(repo.CheckoutDir, args...); err != nil {
				return fmt.Errorf("git %v: %w: %s", args, err, out)
			}
		}
		if _, err := os.Stat(filepath.Join(repo.CheckoutDir, path)); err != nil {
			return fmt.Errorf("main holds no %s to delete: %w", path, err)
		}
		for _, args := range [][]string{{"rm", path}, {"commit", "-m", message}, {"push", "origin", "HEAD:main"}} {
			if out, err := gitRun(repo.CheckoutDir, args...); err != nil {
				return fmt.Errorf("git %v: %w: %s", args, err, out)
			}
		}
		return nil
	}, seedPushTimeout, seedPushInterval).Should(Succeed(), "the outside writer kept losing the push race")
}

var _ = Describe("Bi Directional applier echoes (Flux)", Label("bi-directional", "flux", "applier-echoes"), Ordered,
	func() {
		var (
			testNs, fluxName string
			r                *echoRun
		)

		BeforeAll(func() {
			skipUnlessBiDirectionalEnabled()
			grantAppliersInAnyNamespace()
			testNs = testNamespaceFor("echoes-flux")
			_, _ = kubectlRun("create", "namespace", testNs)
			fluxName = "echoes-" + testNs

			repo := SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-echoes-flux-%d", GinkgoRandomSeed()))
			pipeline := newTimingPipeline(testNs, "flux-echoes", repo)
			pipeline.start()

			applyFluxApplierWithPrune(testNs, repo, fluxName, fluxName+"-git", "main", "./"+pipeline.gitPath, true)
			createFluxClaim(testNs, fluxName, fluxName)
			nameApplierClaim(testNs, pipeline.targetName, "FluxClaim", fluxName, "")
			verifyResourceCondition("gittarget", pipeline.targetName, testNs,
				"ApplierClaimReady", "True", "Succeeded", "")
			verifyResourceCondition(fluxClaimResource, fluxName, testNs, "Held", "True", "Suspended", "")

			r = &echoRun{timingPipeline: pipeline, applier: fluxEchoApplier(testNs, fluxName, repo)}
		})

		AfterAll(func() {
			if !biDirectionalEnabled() || r == nil {
				return
			}
			r.cleanup()
			cleanupNamespacedResource(testNs, fluxClaimResource, fluxName)
			cleanupNamespacedResource("flux-system", "kustomization", fluxName)
			cleanupNamespacedResource("flux-system", "gitrepository", fluxName)
			cleanupNamespacedResource("flux-system", "secret", fluxName+"-git")
			cleanupNamespace(testNs)
		})

		echoScenarios(func() *echoRun { return r })
	})

var _ = Describe("Bi Directional applier echoes (Argo CD)", Label("bi-directional", "argocd", "applier-echoes"),
	Ordered, func() {
		var (
			testNs string
			r      *echoRun
			run    argoBiDirectionalRun
		)

		BeforeAll(func() {
			skipUnlessBiDirectionalEnabled()
			requireArgoCDInstalled()
			grantAppliersInAnyNamespace()
			testNs = testNamespaceFor("echoes-argo")
			_, _ = kubectlRun("create", "namespace", testNs)

			repo := SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-echoes-argo-%d", GinkgoRandomSeed()))
			pipeline := newTimingPipeline(testNs, "argo-echoes", repo)
			run = newArgoBiDirectionalRun(testNs, repo, "argo-echoes")
			pipeline.start()

			run.applyArgoRepoSecret()
			cfg := run.baseAppConfig()
			cfg.Path = pipeline.gitPath
			run.applyArgoApp(cfg)
			createArgoCDClaim(testNs, run.appName, run.argoNs, run.appName)
			nameApplierClaim(testNs, pipeline.targetName, "ArgoCDClaim", run.appName, "")
			verifyResourceCondition("gittarget", pipeline.targetName, testNs,
				"ApplierClaimReady", "True", "Succeeded", "")

			r = &echoRun{timingPipeline: pipeline, applier: argoEchoApplier(run)}
		})

		AfterAll(func() {
			if !biDirectionalEnabled() || r == nil {
				return
			}
			cleanupNamespacedResource(run.argoNs, "application.argoproj.io", run.appName)
			cleanupNamespacedResource(run.argoNs, "secret", run.repoSecretName)
			cleanupNamespacedResource(testNs, argoCDClaimResource, run.appName)
			r.cleanup()
			cleanupNamespace(testNs)
		})

		echoScenarios(func() *echoRun { return r })
	})
