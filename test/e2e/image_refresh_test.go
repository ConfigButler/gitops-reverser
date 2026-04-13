/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// Validates the Task image-refresh dependency chain end-to-end.
//
// Run in isolation:
//
//	go test ./test/e2e/ -v -ginkgo.v --label-filter="image-refresh"
//
// or via:
//
//	task test-image-refresh
var _ = Describe("image refresh dependency chain", Label("image-refresh"), Ordered, func() {
	var (
		projectDir       string
		deployedStamp    string
		imageLoadedStamp string
		savedContent     map[string][]byte
	)

	// revertFile restores a repo-relative path to its content before the test touched it.
	// Uses saved bytes rather than git checkout to preserve any pre-existing uncommitted changes.
	revertFile := func(relPath string) {
		GinkgoHelper()
		original, ok := savedContent[relPath]
		if !ok {
			return
		}
		absPath := filepath.Join(projectDir, relPath)
		Expect(os.WriteFile(absPath, original, 0600)).To(Succeed())
		delete(savedContent, relPath)
	}

	// appendComment saves the original content (once) then appends a harmless comment line.
	appendComment := func(relPath, marker string) {
		GinkgoHelper()
		absPath := filepath.Join(projectDir, relPath)
		if _, alreadySaved := savedContent[relPath]; !alreadySaved {
			original, err := os.ReadFile(absPath)
			Expect(err).NotTo(HaveOccurred())
			savedContent[relPath] = original
		}
		f, err := os.OpenFile(absPath, os.O_APPEND|os.O_WRONLY, 0600)
		Expect(err).NotTo(HaveOccurred())
		if filepath.Base(relPath) == "Dockerfile" {
			_, err = fmt.Fprintf(f, "\nLABEL image-refresh-test=%q\n", marker)
		} else {
			_, err = fmt.Fprintf(f, "\n// image-refresh-test: %s\n", marker)
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Close()).To(Succeed())
	}

	// runPrepare runs task prepare-e2e and returns combined output.
	runPrepare := func() string {
		GinkgoHelper()
		ctx := resolveE2EContext()
		installMode, err := resolveE2EInstallMode()
		Expect(err).NotTo(HaveOccurred())
		cmd := taskCommand(
			fmt.Sprintf("CTX=%s", ctx),
			fmt.Sprintf("NAMESPACE=%s", namespace),
			fmt.Sprintf("INSTALL_MODE=%s", installMode),
			fmt.Sprintf("INSTALL_NAME=%s", resolveE2EInstallName(namespace)),
			"prepare-e2e",
		)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), out)
		_, _ = fmt.Fprintf(GinkgoWriter, "%s", out)
		return out
	}

	// currentPodName returns the name of the running controller pod.
	currentPodName := func() string {
		GinkgoHelper()
		pods, err := controllerPodNames()
		Expect(err).NotTo(HaveOccurred())
		Expect(pods).NotTo(BeEmpty(), "expected at least one controller pod to be running")
		return pods[0]
	}

	// readStamp returns trimmed content of a stamp file.
	readStamp := func(path string) string {
		GinkgoHelper()
		data, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		return strings.TrimSpace(string(data))
	}

	// stampMtime returns the modification time of a stamp file.
	stampMtime := func(path string) time.Time {
		GinkgoHelper()
		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		return info.ModTime()
	}

	BeforeAll(func() {
		if os.Getenv("IMAGE_DELIVERY_MODE") == "pull" {
			Skip("image-refresh tests require IMAGE_DELIVERY_MODE=load; " +
				"they validate the local build → k3d load → rollout restart chain, " +
				"which does not apply when the cluster pulls a pre-built image from a registry")
		}

		savedContent = map[string][]byte{}

		var err error
		projectDir, err = utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())

		ctx := resolveE2EContext()
		deployedStamp = filepath.Join(projectDir, ".stamps", "cluster", ctx, namespace, "controller.deployed")
		imageLoadedStamp = filepath.Join(projectDir, ".stamps", "cluster", ctx, "image.loaded")

		By("ensuring cluster is in a known good state before image-refresh tests")
		runPrepare()
	})

	AfterAll(func() {
		for relPath := range savedContent {
			revertFile(relPath)
		}
	})

	It("S1: no-op run does not restart the pod", func() {
		By("recording stamp mtime and pod name")
		beforeMtime := stampMtime(deployedStamp)
		beforePod := currentPodName()

		// Sleep so that any stamp write after this point would produce a detectably newer mtime.
		time.Sleep(time.Second)

		By("running prepare-e2e with no source changes")
		out := runPrepare()

		By("asserting rollout restart was not issued")
		Expect(out).NotTo(ContainSubstring("Restarting deployment"))

		By("asserting stamp was not touched")
		Expect(stampMtime(deployedStamp)).To(Equal(beforeMtime),
			"controller.deployed stamp should not be re-written when nothing changed")

		By("asserting pod is unchanged")
		Expect(currentPodName()).To(Equal(beforePod))
	})

	It("S2: Go source change triggers pod restart", func() {
		By("recording current pod identity and stamp digest")
		beforePod := currentPodName()
		beforeDigest := readStamp(deployedStamp)

		By("appending a harmless comment to cmd/main.go")
		appendComment("cmd/main.go", "S2")

		By("running prepare-e2e")
		out := runPrepare()

		By("asserting rollout restart was issued")
		Expect(out).To(ContainSubstring("Restarting deployment"))

		By("asserting pod was replaced")
		Expect(currentPodName()).NotTo(Equal(beforePod),
			"expected a new pod after rollout restart")

		By("asserting stamp digest changed")
		Expect(readStamp(deployedStamp)).NotTo(Equal(beforeDigest),
			"controller.deployed stamp should reflect the new image digest")

		By("asserting controller.deployed and image.loaded stamps are in sync")
		Expect(readStamp(deployedStamp)).To(Equal(readStamp(imageLoadedStamp)))
	})

	It("S3: second Go change also triggers restart (not one-shot)", func() {
		By("recording current pod identity and stamp digest")
		beforePod := currentPodName()
		beforeDigest := readStamp(deployedStamp)

		By("appending another harmless comment to cmd/main.go")
		appendComment("cmd/main.go", "S3")

		By("running prepare-e2e")
		out := runPrepare()

		By("asserting rollout restart was issued again")
		Expect(out).To(ContainSubstring("Restarting deployment"))

		By("asserting pod was replaced again")
		Expect(currentPodName()).NotTo(Equal(beforePod),
			"expected a new pod after the second rollout restart")

		By("asserting stamp digest changed again")
		Expect(readStamp(deployedStamp)).NotTo(Equal(beforeDigest))
	})

	It("S4: test-only file change does not trigger rebuild", func() {
		By("reverting Go changes from S2/S3 and re-stabilizing")
		revertFile("cmd/main.go")
		runPrepare() // may rebuild because source content changed back; that is expected here

		By("recording stable stamp mtime and pod name")
		beforeMtime := stampMtime(deployedStamp)
		beforePod := currentPodName()

		// Sleep so that any stamp write after this point would produce a detectably newer mtime.
		time.Sleep(time.Second)

		By("appending a comment to test/e2e/helpers.go (excluded from GO_SOURCES)")
		appendComment("test/e2e/helpers.go", "S4")

		By("running prepare-e2e")
		out := runPrepare()

		By("asserting stamp was not touched")
		Expect(stampMtime(deployedStamp)).To(Equal(beforeMtime),
			"controller.deployed stamp should not change for a test-only file modification")

		By("asserting rollout restart was not issued")
		Expect(out).NotTo(ContainSubstring("Restarting deployment"))

		By("asserting pod is unchanged")
		Expect(currentPodName()).To(Equal(beforePod))
	})

	It("S5: Dockerfile change triggers rebuild", func() {
		By("recording current pod identity")
		beforePod := currentPodName()

		By("appending a harmless comment to Dockerfile")
		appendComment("Dockerfile", "S5")

		By("running prepare-e2e")
		out := runPrepare()

		By("asserting rollout restart was issued")
		Expect(out).To(ContainSubstring("Restarting deployment"))

		By("asserting pod was replaced")
		Expect(currentPodName()).NotTo(Equal(beforePod),
			"expected a new pod after Dockerfile change")
	})

	It("S6: stamp content matches the digest of the running pod", func() {
		By("reading stamp content")
		deployed := readStamp(deployedStamp)
		loaded := readStamp(imageLoadedStamp)

		By("asserting controller.deployed and image.loaded stamps are in sync")
		Expect(deployed).To(Equal(loaded))

		By("asserting stamp has the expected IMAGE@sha256:DIGEST format")
		Expect(deployed).To(MatchRegexp(`^.+@sha256:[a-f0-9]+$`),
			"controller.deployed should contain an image reference and a sha256 digest")

		By("reading the running pod's imageID from the cluster")
		podImageID, err := kubectlRunInNamespace(namespace,
			"get", "pods",
			"-l", controllerPodLabelSelector,
			"--sort-by=.status.startTime",
			"-o", "jsonpath={.items[-1:].status.containerStatuses[0].imageID}",
		)
		Expect(err).NotTo(HaveOccurred())

		By("asserting the pod's imageID contains the digest recorded in the stamp")
		deployedDigest := deployed[strings.LastIndex(deployed, "@")+1:]
		Expect(podImageID).To(ContainSubstring(deployedDigest),
			"the running pod should use the image whose digest is recorded in controller.deployed")
	})
})
