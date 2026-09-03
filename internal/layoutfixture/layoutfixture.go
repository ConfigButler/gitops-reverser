// SPDX-License-Identifier: Apache-2.0

// Package layoutfixture reads the expected-*-status.yaml fixtures under test/fixtures/layout-corpus.
//
// It exists because those fixtures are asserted from two packages that cannot share a test
// helper: internal/git pins the half a refusal produces (GitPathAccepted), and
// internal/controller pins the half the controller projects from it (LayoutResolved, Stalled).
// A fixture parser copied into both would be two things that must agree about a file format,
// which is the drift the fixtures exist to prevent.
package layoutfixture

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Root is test/fixtures/layout-corpus as reached from a package directory two levels below the
// repository root
// (internal/git, internal/controller). The fixtures are read in place rather than copied into a
// testdata directory: a copy would drift from the documents it illustrates, and the drift would
// be invisible in review.
const Root = "../../test/fixtures/layout-corpus"

// Condition is one expected condition in a fixture.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type statusFixture struct {
	Status struct {
		Conditions []Condition `json:"conditions"`
	} `json:"status"`
}

// Path joins a fixture path onto Root.
func Path(elem ...string) string {
	return filepath.Join(append([]string{Root}, elem...)...)
}

// ReadCondition returns the named condition from a status fixture. A missing condition is an
// error rather than a zero value: a test asking for one is asserting that the fixture makes that
// claim, and a silently absent claim is exactly what would let the fixture rot.
func ReadCondition(path, conditionType string) (Condition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Condition{}, err
	}
	var fixture statusFixture
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		return Condition{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	for _, condition := range fixture.Status.Conditions {
		if condition.Type == conditionType {
			return condition, nil
		}
	}
	return Condition{}, fmt.Errorf("%s has no %s condition, so nothing pins it", path, conditionType)
}
