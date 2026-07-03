// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
)

const defaultE2ENamespace = "gitops-reverser"

func resolveE2ENamespace() string {
	if value := strings.TrimSpace(os.Getenv("NAMESPACE")); value != "" {
		return value
	}
	return defaultE2ENamespace
}

// testNamespaceFor returns the test namespace for a given suite label.
// The name is scoped to the current Ginkgo run via the random seed,
// tying it to the repo and other per-run resources.
// Example: "8675309-test-manager"
// If TESTNAMESPACE is set, that value is returned directly (used by demo runs to pin to "vote").
func testNamespaceFor(suite string) string {
	if value := strings.TrimSpace(os.Getenv("TESTNAMESPACE")); value != "" {
		return value
	}
	return fmt.Sprintf("%d-test-%s", GinkgoRandomSeed(), suite)
}
