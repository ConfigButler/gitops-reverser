// SPDX-License-Identifier: Apache-2.0

// Package manifestanalyzer is the public, stable answer to two questions a tool built
// around GitOps Reverser needs to ask about a Git repository, without a cluster and
// without writing anything:
//
//	ScanFolder — may this folder become a GitTarget, and if not, why?
//	ScanRepo   — which folders in this repository could, and what shape is each one?
//
// The decisions come from the same acceptance gate the operator's writer enforces before
// it commits a byte, so a tool built on this package cannot drift from the operator that
// will later refuse the folder. Nothing here re-implements a rule.
//
// # Stability
//
// This package is the supported surface. The types below carry JSON tags and a
// [SchemaVersion]; fields are added, never repurposed or removed within a schema major.
// Everything under internal/ is not covered by that promise and is not importable from
// another module.
//
// The command-line equivalents are `manifest-analyzer --mode scan --format json` and
// `--mode repo-walker --format json`, which emit exactly the documents [FolderReport]
// and [RepoReport] marshal to. Exec the binary if Go is not your language; import this
// package if it is.
//
// # What it does not do
//
// Neither entry point resolves types against a live cluster, so neither reports whether a
// document's kind is actually served, nor produces a write plan. Both are structure-only:
// they read bytes, never follow symlinks, and never write. The operator applies the same
// gate plus the cluster-aware checks when a GitTarget adopts the folder — a folder this
// package accepts can still be refused for a reason only a cluster can see (an unresolved
// kind, an out-of-scope resource).
package manifestanalyzer
