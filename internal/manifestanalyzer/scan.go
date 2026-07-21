// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"fmt"
	"io/fs"
	"os"

	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// Scan is the M5 dry-run: the one planner shared by the manifest-analyzer CLI and
// the controller's scan path, described in
// docs/spec/current-manifest-support-review.md ("Scan Mode (Dry-Run)").
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
	lookup typeset.Lookup,
	desired []DesiredResource,
	policy ScanPolicy,
) ScanResult {
	scan := collectFiles(fsys)
	store := buildStore(ctx, scan, lookup, policy.Acceptance.Allowlist)
	acc := Accept(store, policy.Acceptance)
	plan := BuildPlan(store, scan.YAMLFiles, desired, policy.Plan)
	return ScanResult{Store: store, Acceptance: acc, Plan: plan}
}

// ScanDir is Scan over the directory at root (the CLI entry point). It verifies root
// is a directory, then scans os.DirFS(root). Symlinks are never followed.
func ScanDir(
	ctx context.Context,
	root string,
	lookup typeset.Lookup,
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
	res := Scan(ctx, os.DirFS(root), lookup, desired, policy)
	res.Store.Root = root
	return res, nil
}

// ScanPolicy bundles the acceptance and planning policy for a dry-run scan, so a
// caller configures the whole pipeline in one value.
type ScanPolicy struct {
	// Acceptance configures the adoption gate (allowlist + scope). Its allowlist also
	// drives store construction, so allowlisted documents are retained, not planned.
	Acceptance AcceptancePolicy
	// Plan configures the planner (projection + edit options). A dry-run has no GitTarget
	// and therefore no prune policy to read, so a caller that wants the folder's orphans
	// listed must set Plan.Sweep to SweepDropOrphans deliberately — see FolderScanPlanPolicy.
	Plan Policy
}

// FolderScanPlanPolicy is the planning policy for an offline folder scan: it DROPS orphans.
//
// A scan is a report, not a write, and it is run against a folder rather than against a
// GitTarget — there is no spec.prune to consult. Reporting the full convergence view is the
// analysis the tool exists to produce; suppressing it would make a folder full of stale
// documents render identically to a converged one, with nothing on screen to distinguish them.
// Nothing here can delete anything: Scan writes nothing, and the live writer builds its own
// policy from the target.
//
// In practice a CLI scan also passes a nil mapper, so no document resolves to MappingFollowable
// and no drop is emitted regardless. This is the deliberate choice for the day that changes.
func FolderScanPlanPolicy() Policy {
	return Policy{Sweep: SweepDropOrphans}
}

// ScanResult is the dry-run outcome: the built store, the acceptance decision, and
// the full plan. It carries everything needed to render the human, JSON, and status
// views without recomputation.
type ScanResult struct {
	Store      *ManifestStore
	Acceptance Acceptance
	Plan       Plan
}
