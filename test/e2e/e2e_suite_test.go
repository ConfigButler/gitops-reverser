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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

var (
	// projectImage is the name of the image to be tested.
	// In CI, this is provided via PROJECT_IMAGE environment variable.
	// For local testing, defaults to locally built image.
	projectImage = getProjectImage()
)

// getProjectImage returns the project image name from environment or default.
func getProjectImage() string {
	if img := os.Getenv("PROJECT_IMAGE"); img != "" {
		return img
	}
	return "gitops-reverser:e2e-local"
}

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
	// Check if image is pre-built (CI environment)
	if os.Getenv("PROJECT_IMAGE") != "" {
		By(fmt.Sprintf("using pre-built image: %s", projectImage))
		// Image is already built and loaded in CI pipeline
		return
	}

	// IDE/direct go test path: ensure cluster exists and local image is built+loaded via Makefile.
	By("PROJECT_IMAGE is not set; preparing cluster/image through Makefile for local run")
	cmd := exec.Command("make", "setup-cluster", "e2e-build-load-image", fmt.Sprintf("PROJECT_IMAGE=%s", projectImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build/load manager image via Makefile")
})

var _ = AfterSuite(func() {
})
