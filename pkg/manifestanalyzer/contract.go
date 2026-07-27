// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"runtime/debug"
)

// The report envelope. A report is a KRM document — apiVersion, kind, spec, status — and
// not a bespoke JSON shape, because the three questions a bespoke shape leaves open
// ("what does a version bump assert", "must a reader refuse a version it does not know",
// "what bumps it at all") are already answered by the Kubernetes API conventions every
// consumer of a GitOps tool has read. Citing that document is cheaper than writing one.
//
// [APIVersion] replaces the former SchemaVersion marker outright. It follows the
// alpha/beta/GA contract: an alpha version may change incompatibly in any release, and a
// reader that does not know the version it is handed must refuse it rather than
// best-effort parse — the same rule every Kubernetes client already follows. Adding a
// field is still not a version bump, so ignore fields you do not recognise.
const (
	// APIVersion is the group/version of both report kinds.
	APIVersion = "manifestanalyzer.configbutler.ai/v1alpha1"
	// KindFolderReport is the kind of a [FolderReport].
	KindFolderReport = "FolderReport"
	// KindRepoReport is the kind of a [RepoReport].
	KindRepoReport = "RepoReport"
)

// Scan modes, as reported in a report's spec.
const (
	// ModeScanFolder is the mode of a [FolderReport]: may THIS folder become a GitTarget?
	ModeScanFolder = "scan-folder"
	// ModeScanRepo is the mode of a [RepoReport]: which folders in this repository could?
	ModeScanRepo = "scan-repo"
)

// TypeMeta is the KRM envelope every report carries. It is deliberately shaped like
// apimachinery's TypeMeta so a consumer that does link us can mirror or reuse it.
//
// A report is NEVER served and NEVER registered: there is no CRD, no group registration,
// and it cannot be applied. It observes a path at an instant. That is also why it carries
// no metadata — a report has no identity, a synthesized name would be noise, and worse, it
// would suggest the document can be applied. Kpt's own kind: ResourceList carries
// apiVersion, kind and its payload with no metadata, for the same reason.
type TypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// Generator names the build that produced a report. Never empty: a report that cannot say
// what produced it is the failure this field exists to prevent. A tool that execs the
// binary holds the JSON document and nothing else — without this it cannot tell a report
// from one release consumed against a writer from another, which is a wrong answer that
// looks entirely normal.
//
// A bare version string would not do: kind names the document, Name names what produced
// it, and a report piped into another tool is exactly where conflating the two bites.
type Generator struct {
	// Name is the producing tool, e.g. "manifest-analyzer".
	Name string `json:"name"`
	// Version is the release, e.g. "v0.39.1", or "dev" for a build carrying no version.
	Version string `json:"version"`
}

// GeneratorName is the tool name every report carries. It names the analyzer, not the
// binary that hosts it: a report produced by the library linked into another program
// still says manifest-analyzer, because that is what decided the contents.
const GeneratorName = "manifest-analyzer"

// version is overridden at link time with -ldflags "-X
// github.com/ConfigButler/gitops-reverser/pkg/manifestanalyzer.version=vX.Y.Z" by the
// release build. It is empty for every other build, which is not a problem: [Version]
// falls back to the module version the Go toolchain already records.
var version string

// modulePath is this module's path, used to find our own version in the build info of a
// binary that merely links us.
const modulePath = "github.com/ConfigButler/gitops-reverser"

// Version reports the release that produced a report, resolved in three steps: the
// ldflags-injected version if the build set one; otherwise the module version
// runtime/debug records — which is what `go install ...@vX.Y.Z` (the install path this
// package's own documentation recommends) produces for free, with no build change and no
// release-workflow change; otherwise the literal "dev".
//
// It is never empty, so a consumer may read an ABSENT generator as "produced before this
// field shipped" without having to distinguish that from "produced by a build that did
// not know itself".
func Version() string {
	if version != "" {
		return version
	}
	if v, ok := moduleVersion(); ok {
		return v
	}
	return "dev"
}

// moduleVersion digs this module's version out of the embedded build info. The binary may
// BE this module (go install / go build of cmd/manifest-analyzer), in which case
// Main.Version carries it, or it may merely link this package, in which case our version
// is one of the dependency entries and Main names the consumer instead.
func moduleVersion() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	if info.Main.Path == modulePath {
		return usableVersion(info.Main.Version)
	}
	for _, dep := range info.Deps {
		if dep != nil && dep.Path == modulePath {
			return usableVersion(dep.Version)
		}
	}
	return "", false
}

// usableVersion rejects the placeholders the toolchain records for a build with no module
// version — a plain `go build` in a checkout reports "(devel)", and an unresolved
// dependency reports nothing at all.
func usableVersion(v string) (string, bool) {
	if v == "" || v == "(devel)" {
		return "", false
	}
	return v, true
}

// generator builds the [Generator] every report carries.
func generator() Generator {
	return Generator{Name: GeneratorName, Version: Version()}
}
