// SPDX-License-Identifier: Apache-2.0

// Package manifestanalyzer is the public answer to two questions a tool built
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
// # Stability: none yet
//
// GitOps Reverser is pre-1.0, and so is this package. It is the surface a tool is meant
// to build on rather than reaching into internal/, but it carries no compatibility
// guarantee: fields may be renamed, removed, or given a new meaning in any release. Pin a version: each
// release is tagged `vX.Y.Z`, so `go get github.com/ConfigButler/gitops-reverser@vX.Y.Z` (and
// `go install github.com/ConfigButler/gitops-reverser/cmd/manifest-analyzer@vX.Y.Z`) resolve to
// that release rather than an opaque pseudo-version — the whole repository is one Go module,
// versioned as a unit.
//
// Two habits will nonetheless save you work. Ignore fields you do not recognise, because
// new ones do get added. Do not switch on the human-readable strings — [Issue.Message]
// and [RefusalReason.Detail] are prose, while [IssueKind] and the refusal reason codes
// are the values worth matching on.
//
// # The report is a KRM document, and it says what produced it
//
// Both reports carry [APIVersion] and a kind, the scan request in spec and the findings in
// status. A reader that does not know the apiVersion it is handed should refuse it rather
// than best-effort parse, by the same rule every Kubernetes client already follows; adding
// a field is not a version bump. The document is never served and never applyable — see
// [TypeMeta].
//
// [FolderReportStatus.Generator] and [RepoReportStatus.Generator] name the build that
// produced the report, so a document that outlives the process that made it still says
// which release decided its contents. `manifest-analyzer --version` prints the same pair.
//
// # A refusal says whether it can be solved
//
// [Issue] and [RefusalReason] carry a [Solvability] — "yes" or "no" — and, when someone
// can act, an [Actor]. A code alone cannot tell "one broken document away from working"
// from "this folder cannot be adopted", and guessing from the code is how a consumer ends
// up telling a user to go fix something only their platform team can, or nothing at all.
// The answer describes this release and makes no promise about the future, so read it on
// every scan rather than caching a mapping from it. Treat an absent or unrecognised value
// as [SolvabilityUnknown] and say nothing.
//
// Everything under internal/ carries no guarantee either, and is not importable from
// another module. One format from there is nonetheless a contract you may build on: a
// resource's identity key is "{group}/{version}/{resource}/{namespace}/{name}", with the
// namespace segment dropped (not emitted empty) for a cluster-scoped resource and an empty
// group segment for core resources, so the four shapes are "apps/v1/deployments/prod/api",
// "rbac.authorization.k8s.io/v1/clusterroles/admin", "/v1/secrets/prod/db" and
// "/v1/nodes/node-1". It is specified and golden-tested at
// ResourceIdentifier.Key in internal/types/identifier.go, which also records why a join
// that must survive a storage-version bump keys on the versionless Git path instead.
//
// The command-line equivalents are `manifest-analyzer --mode scan-folder --format json` and
// `--mode scan-repo --format json`, which emit exactly the documents [FolderReport]
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
