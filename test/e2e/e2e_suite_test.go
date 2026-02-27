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
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

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
	seed := ginkgoRandomSeed()
	ns := resolveE2ENamespace()
	installName := resolveE2EInstallName(seed)
	target := fmt.Sprintf(".stamps/cluster/%s/%s/e2e/prepare", ctx, ns)
	cmd := makeCommand(
		fmt.Sprintf("CTX=%s", ctx),
		fmt.Sprintf("INSTALL_NAME=%s", installName),
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

	By("ensuring IceCreamOrder CRD is removed before tests")
	deleteIceCreamOrderCRD := exec.Command(
		"kubectl",
		"--context", ctx,
		"delete", "crd", "icecreamorders.shop.example.com",
		"--ignore-not-found=true",
	)
	output, err = utils.Run(deleteIceCreamOrderCRD)
	Expect(err).NotTo(HaveOccurred(), "failed to delete IceCreamOrder CRD before tests")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)

	// The Makefile prepares the age key under the stamp directory. When running `go test` directly (without
	// `make test-e2e`), ensure the suite uses that prepared key file.
	if strings.TrimSpace(os.Getenv("E2E_AGE_KEY_FILE")) == "" {
		wd, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred(), "failed to get working directory for e2e run")
		ageKeyPath := filepath.Join(wd, ".stamps", "cluster", ctx, "age-key.txt")
		Expect(os.Setenv("E2E_AGE_KEY_FILE", ageKeyPath)).To(Succeed())
	}
}

func makeCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("make", args...)
	_, _ = fmt.Fprintf(
		GinkgoWriter,
		"make invocation: args=%v\n",
		args,
	)
	return cmd
}

func resolveE2EInstallName(seed int64) string {
	if value := strings.TrimSpace(os.Getenv("INSTALL_NAME")); value != "" {
		return value
	}
	return fmt.Sprintf("gitops-reverser-%d", seed)
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
