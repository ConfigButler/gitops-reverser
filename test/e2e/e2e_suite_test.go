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
	return "example.com/gitops-reverser:v0.0.1"
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

	// Local testing: ALWAYS rebuild to ensure latest code changes are included
	By("building the manager(Operator) image for local testing (forcing rebuild)")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	By("loading the manager(Operator) image on Kind (forcing reload)")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")
})

var _ = AfterSuite(func() {
})
