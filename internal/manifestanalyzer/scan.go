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

package manifestanalyzer

import (
	"context"
	"fmt"
	"io/fs"
	"os"

	"github.com/ConfigButler/gitops-reverser/internal/mapping"
)

// Scan is the M5 dry-run: the one planner shared by the manifest-analyzer CLI and
// the controller's scan path, described in
// docs/design/manifest/current-manifest-support-review.md ("Scan Mode (Dry-Run)").
// It builds the store (applying the policy's allowlist), runs the acceptance gate,
// and computes the full plan against the desired set — then stops. It writes
// nothing.
//
// The plan is ALWAYS computed, even when acceptance refuses, so an operator can see
// exactly what reconcile would do (creates, patches, managed drops) alongside the
// reasons a folder would be rejected. Whether to act on the plan is gated on
// Acceptance.Accepted by the caller: the live writer (M7) applies a plan only for an
// accepted folder. desired must be the COMPLETE desired snapshot (the planner
// mark-and-sweeps); pass nil for a structure-only scan with no cluster.
func Scan(
	ctx context.Context,
	fsys fs.FS,
	mapper mapping.ResourceMapper,
	desired []DesiredResource,
	policy ScanPolicy,
) ScanResult {
	yamlFiles, _, scanDiags := collectFiles(fsys)
	store := buildStore(ctx, yamlFiles, scanDiags, mapper, policy.Acceptance.Allowlist)
	acc := Accept(store, policy.Acceptance)
	plan := BuildPlan(store, yamlFiles, desired, policy.Plan)
	return ScanResult{Store: store, Acceptance: acc, Plan: plan}
}

// ScanDir is Scan over the directory at root (the CLI entry point). It verifies root
// is a directory, then scans os.DirFS(root). Symlinks are never followed.
func ScanDir(
	ctx context.Context,
	root string,
	mapper mapping.ResourceMapper,
	desired []DesiredResource,
	policy ScanPolicy,
) (ScanResult, error) {
	info, err := os.Stat(root)
	if err != nil {
		return ScanResult{}, err
	}
	if !info.IsDir() {
		return ScanResult{}, fmt.Errorf("not a directory: %s", root)
	}
	res := Scan(ctx, os.DirFS(root), mapper, desired, policy)
	res.Store.Root = root
	return res, nil
}

// ScanPolicy bundles the acceptance and planning policy for a dry-run scan, so a
// caller configures the whole pipeline in one value.
type ScanPolicy struct {
	// Acceptance configures the adoption gate (allowlist + scope). Its allowlist also
	// drives store construction, so allowlisted documents are retained, not planned.
	Acceptance AcceptancePolicy
	// Plan configures the planner (projection + edit options).
	Plan Policy
}

// ScanResult is the dry-run outcome: the built store, the acceptance decision, and
// the full plan. It carries everything needed to render the human, JSON, and status
// views without recomputation.
type ScanResult struct {
	Store      *ManifestStore
	Acceptance Acceptance
	Plan       Plan
}
