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
	"path"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func pullLatestRepoState(g Gomega, checkoutDir string) {
	GinkgoHelper()

	output, err := gitRun(checkoutDir, "pull", "--ff-only")
	if err != nil {
		g.Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Should successfully pull latest changes. Output: %s", output))
	}
}

func assertLatestCommitTouchesOnly(g Gomega, checkoutDir string, expectedPaths []string) {
	GinkgoHelper()

	output, err := gitRun(checkoutDir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	g.Expect(err).NotTo(HaveOccurred(), "Should read changed paths for latest commit")

	actualPaths := nonEmptyTrimmedLines(output)
	sort.Strings(actualPaths)

	expected := append([]string(nil), expectedPaths...)
	sort.Strings(expected)

	g.Expect(actualPaths).To(Equal(expected),
		fmt.Sprintf("Latest commit should touch only expected paths. Actual: %v", actualPaths))
}

func assertLatestCommitTouchesNoNamespaces(
	g Gomega,
	checkoutDir string,
	basePath string,
	forbiddenNamespaces []string,
) {
	GinkgoHelper()

	output, err := gitRun(checkoutDir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	g.Expect(err).NotTo(HaveOccurred(), "Should read changed paths for latest commit")

	actualPaths := nonEmptyTrimmedLines(output)
	for _, changedPath := range actualPaths {
		for _, namespace := range forbiddenNamespaces {
			forbiddenPrefix := path.Join(basePath, namespace) + "/"
			g.Expect(changedPath).NotTo(HavePrefix(forbiddenPrefix),
				fmt.Sprintf("Latest commit should not touch forbidden namespace path %q", forbiddenPrefix))
		}
	}
}

func nonEmptyTrimmedLines(input string) []string {
	lines := strings.Split(input, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}
