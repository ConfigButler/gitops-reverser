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
	"os/exec"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

const e2eGinkgoSeedEnv = "E2E_GINKGO_RANDOM_SEED"

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purpose of being used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting gitops-reverser integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	if img := os.Getenv("PROJECT_IMAGE"); img == "" {
		By("local run: preparing cluster via Makefile target")
	} else {
		By(fmt.Sprintf("using pre-built image: %s", img))
	}

	By("preparing e2e prerequisites via Makefile target")
	ensureE2EPrepared()
})

var _ = AfterSuite(func() {
})

func ensureE2EPrepared() {
	ctx := resolveE2EContext()
	ns := resolveE2ENamespace()
	target := fmt.Sprintf(".stamps/cluster/%s/%s/e2e/prepare", ctx, ns)
	cmd := makeCommandWithSeed(
		fmt.Sprintf("CTX=%s", ctx),
		fmt.Sprintf("NAMESPACE=%s", ns),
		target,
		"portforward-ensure",
	)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to run make target for e2e prepare")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)

	// Many e2e helpers invoke kubectl without an explicit --context flag. Ensure kubectl is pointed at the
	// intended cluster context for the remainder of the test run.
	useCtx := exec.Command("kubectl", "config", "use-context", ctx)
	output, err = utils.Run(useCtx)
	Expect(err).NotTo(HaveOccurred(), "failed to set kubectl context for e2e run")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)
}

func makeCommandWithSeed(args ...string) *exec.Cmd {
	seed := ginkgoRandomSeed()
	seedPrefix := e2eGinkgoSeedEnv + "="
	filteredArgs := make([]string, 0, len(args)+1)
	for _, arg := range args {
		if strings.HasPrefix(arg, seedPrefix) {
			continue
		}
		filteredArgs = append(filteredArgs, arg)
	}
	filteredArgs = append(filteredArgs, fmt.Sprintf("%s=%d", e2eGinkgoSeedEnv, seed))
	cmd := exec.Command("make", filteredArgs...)
	_, _ = fmt.Fprintf(
		GinkgoWriter,
		"make invocation: seed=%d arg=%s args=%v\n",
		seed,
		fmt.Sprintf("%s=%d", e2eGinkgoSeedEnv, seed),
		filteredArgs,
	)
	return cmd
}

func resolveE2ENamespace() string {
	if value := strings.TrimSpace(os.Getenv("NAMESPACE")); value != "" {
		return value
	}
	return "gitops-reverser" // does it wrok?
}

func ginkgoRandomSeed() int64 {
	suiteConfig, _ := GinkgoConfiguration()
	return suiteConfig.RandomSeed
}

func resolveE2EContext() string {
	if value := strings.TrimSpace(os.Getenv("CTX")); value != "" {
		return value
	}

	if cluster := strings.TrimSpace(os.Getenv("KIND_CLUSTER")); cluster != "" {
		if strings.HasPrefix(cluster, "kind-") {
			return cluster
		}
		return fmt.Sprintf("kind-%s", cluster)
	}

	cmd := exec.Command("kubectl", "config", "current-context")
	output, err := utils.Run(cmd)
	if err == nil {
		if value := strings.TrimSpace(output); value != "" {
			return value
		}
	}

	return "kind-gitops-reverser-test-e2e"
}

/*

At startup of tests we should define the seed of the test run and use that to create a namespace and to run tests in it, also the build should be depending on that.

We should be cominbing instllation methods with testsuites

helm
manifests
config (is there still use for this?!)

full
quickstart

*/
