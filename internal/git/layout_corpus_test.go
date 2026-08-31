// SPDX-License-Identifier: Apache-2.0

package git

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/format/diff"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// The layout corpus executes the worked examples under docs/layout/. Until this file
// existed those folders were read by nothing but a human, so every claim in them was
// prose: the READMEs said where a document lands and what the commit looks like, and
// nothing failed when the writer disagreed. Each scenario now seeds a worktree from
// `repository/`, folds `input/` through the real plan-then-flush path with the flush
// policy derived from `config/gittarget.yaml`, and compares a normalized diff against
// `expected-*.patch`.
//
// Three conventions here are load-bearing and are stated in
// docs/layout/shapes/README.md as well:
//
//   - A scenario describing behavior that is not built yet is written NOW and skipped,
//     naming the PR that unskips it. The corpus is the definition of done for that PR:
//     it is finished when the last skip is gone.
//   - `config/gittarget.yaml` parses into corpusGitTarget below, a HARNESS-LOCAL struct,
//     for exactly as long as it names fields the API does not have. Every field it holds
//     that v1alpha3.GitTargetSpec also holds is asserted against the real type by
//     TestLayoutCorpus_ConfigParsesAgainstTheRealAPI, so the examples cannot quietly
//     describe an API nobody built.
//   - Refusals are fixtures too. A scenario set where every write succeeds is
//     advertising rather than specification, so the refusal halves assert an
//     `expected-status.yaml` instead of a patch.
//
// Run with -update to rewrite the expected patches from the observed diff.

var updateLayoutCorpus = flag.Bool("update", false,
	"rewrite docs/layout expected-*.patch fixtures from the observed diff")

// layoutCorpusRoot is docs/layout/ as reached from this package's directory. The
// fixtures are read in place rather than copied into testdata/: a copy would drift from
// the documents it illustrates, and the drift would be invisible in review.
const layoutCorpusRoot = "../../docs/layout"

// corpusGitTarget is the harness-local reading of a scenario's config/gittarget.yaml.
//
// It is deliberately NOT v1alpha3.GitTarget. The examples use spec.serializeNamespace
// and spec.placement.useKustomize, which PR 2 introduces, so decoding into the real type
// would either fail or silently drop them. The mapping is temporary by design and PR 2
// deletes it — the pointer-typed booleans below are the fields that keep it alive, and
// when they move to the real spec this struct has nothing left to hold.
type corpusGitTarget struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Path    string `json:"path"`
		Branch  string `json:"branch"`
		Suspend bool   `json:"suspend"`
		// SerializeNamespace is PR 2's spec.serializeNamespace: unset means infer.
		SerializeNamespace *bool `json:"serializeNamespace"`
		Placement          *struct {
			ByType  map[string]string `json:"byType"`
			Default string            `json:"default"`
			// UseKustomize is PR 2's placement.useKustomize.
			UseKustomize *bool `json:"useKustomize"`
		} `json:"placement"`
	} `json:"spec"`
}

// policy projects the parsed config onto the flush policy the write path actually takes
// today. Only the two shipped rungs — byType and default — cross over; the two booleans
// have no consumer until PR 2, which is precisely why the scenarios that depend on them
// are skipped rather than asserted.
func (c corpusGitTarget) policy() *manifestanalyzer.PlacementPolicy {
	if c.Spec.Placement == nil {
		return nil
	}
	if len(c.Spec.Placement.ByType) == 0 && c.Spec.Placement.Default == "" {
		return nil
	}
	return &manifestanalyzer.PlacementPolicy{
		ByType:  c.Spec.Placement.ByType,
		Default: c.Spec.Placement.Default,
	}
}

// corpusScenario is one executable row of the corpus: a fixture folder, which config in
// it drives the write, which input object arrives, and what Git is expected to look like
// afterwards.
type corpusScenario struct {
	// dir is the fixture folder, relative to docs/layout.
	dir string
	// config names the GitTarget under config/; folders with one target may omit it.
	config string
	// input names the live object under input/.
	input string
	// patch names the expected diff under the fixture folder. Empty means the scenario
	// asserts a refusal instead, through wantRefusal.
	patch string
	// wantRefusal, when set, is a substring the flush error must contain. It is the
	// executable half of an expected-status.yaml: the condition text is a projection of
	// this refusal, and the projection is asserted in the controller's own tests.
	wantRefusal string
	// skip names the PR that unskips this scenario, and is the whole reason the row is
	// written before the behavior exists. An empty skip is a scenario that runs today.
	skip string
}

func (s corpusScenario) configFile() string {
	if s.config != "" {
		return s.config
	}
	return "gittarget.yaml"
}

func (s corpusScenario) name() string {
	if s.config == "" || s.config == "gittarget.yaml" {
		return s.dir
	}
	return s.dir + "/" + strings.TrimSuffix(s.config, ".yaml")
}

// layoutCorpus is the whole corpus. The eight folder shapes are the cross-product of
// docs/layout/shapes/README.md; the two specific examples are the Argo CD and Flux
// repositories of docs/layout/specific-examples/README.md.
func layoutCorpus() []corpusScenario {
	return []corpusScenario{
		{
			dir:   "shapes/1-flat-serialized",
			input: "checkout-config.yaml",
			patch: "expected-checkout-config.patch",
		},
		{
			dir:   "shapes/2-flat-namespace-free",
			input: "checkout-config.yaml",
			patch: "expected-checkout-config.patch",
			skip: "PR 2: needs spec.serializeNamespace: false. Today inference writes " +
				"metadata.namespace because no kustomization in the folder supplies it",
		},
		{
			dir:   "shapes/3-tree-serialized",
			input: "checkout-config.yaml",
			patch: "expected-checkout-config.patch",
		},
		{
			dir:   "shapes/4-tree-namespace-free",
			input: "checkout-config.yaml",
			patch: "expected-checkout-config.patch",
			skip: "PR 2: needs spec.serializeNamespace: false. Today inference writes " +
				"metadata.namespace because no kustomization in the folder supplies it",
		},
		{
			dir:   "shapes/5-kustomize-single-folder",
			input: "checkout-config.yaml",
			patch: "expected-checkout-config.patch",
		},
		{
			dir:    "shapes/5-kustomize-single-folder",
			config: "gittarget-empty-folder.yaml",
			input:  "checkout-config.yaml",
			patch:  "expected-empty-folder-first-write.patch",
			skip: "PR 2: needs placement.useKustomize to create the kustomization.yaml this " +
				"empty folder has none of",
		},
		{
			dir:    "shapes/6-kustomize-base-and-overlays",
			config: "gittarget-prod.yaml",
			input:  "checkout-config.yaml",
			patch:  "expected-checkout-config.patch",
		},
		{
			dir:    "shapes/7-kustomize-layered",
			config: "gittarget-prod.yaml",
			input:  "checkout-config.yaml",
			patch:  "expected-checkout-config.patch",
		},
		{
			dir:    "shapes/8-base-owned-field-edit",
			config: "gittarget-prod.yaml",
			input:  "deployment-image-bumped.yaml",
			patch:  "expected-image-bump.patch",
			skip: "patch authoring (track C of docs/design/build-order.md): writing an " +
				"images: declaration into the overlay is not built, and is not PR 2 either",
		},
		{
			// The refusal half of shape 8, and the reason the shape is in the set at all: the
			// changed field (a container env var) is expressed only in the base, which this
			// target reads and never writes. expected-env-change-status.yaml is the condition
			// a user reads; the refusal below is what produces it.
			dir:         "shapes/8-base-owned-field-edit",
			config:      "gittarget-prod.yaml",
			input:       "deployment-env-changed.yaml",
			wantRefusal: "escapes the GitTarget write scope",
		},
		{
			dir:   "specific-examples/homelab-argocd",
			input: "paperless.yaml",
			patch: "expected-paperless.patch",
		},
		{
			dir:   "specific-examples/homelab-flux",
			input: "bitnami.yaml",
			patch: "expected-bitnami.patch",
		},
	}
}

// TestLayoutCorpus runs every scenario in docs/layout/ against the real write path.
func TestLayoutCorpus(t *testing.T) {
	for _, sc := range layoutCorpus() {
		t.Run(sc.name(), func(t *testing.T) {
			if sc.skip != "" {
				t.Skip(sc.skip)
			}
			runCorpusScenario(t, sc)
		})
	}
}

func runCorpusScenario(t *testing.T, sc corpusScenario) {
	t.Helper()
	folder := filepath.Join(layoutCorpusRoot, sc.dir)

	target := readCorpusGitTarget(t, filepath.Join(folder, "config", sc.configFile()))
	obj := readCorpusInput(t, filepath.Join(folder, "input", sc.input))

	worktree, seeded := seedCorpusWorktree(t, filepath.Join(folder, "repository"))
	event := corpusEvent(t, obj, target)

	worker := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: corpusMapper()}
	_, err := worker.flushEventsToWorktree(
		t.Context(), worktree, sanitizePath(target.Spec.Path),
		[]Event{event}, target.policy(), v1alpha3.PruneOnEvent)

	if sc.patch == "" {
		requireCorpusRefusal(t, err, sc.wantRefusal, worktree, seeded)
		return
	}
	require.NoError(t, err, "%s: the scenario expects a commit, not a refusal", sc.name())

	got := corpusDiff(t, worktree, seeded)
	assertCorpusPatch(t, filepath.Join(folder, sc.patch), got)
}

// requireCorpusRefusal asserts a scenario that must not write. Refusing is only half of
// it: a refusal that leaves bytes behind is a partial commit, so the worktree has to come
// back exactly as it was seeded.
func requireCorpusRefusal(t *testing.T, err error, want string, worktree *gogit.Worktree, seeded *object.Commit) {
	t.Helper()
	require.Error(t, err, "the scenario expects a refusal")
	if want != "" {
		require.Contains(t, err.Error(), want)
	}
	require.Empty(t, corpusDiff(t, worktree, seeded), "a refused flush must leave the folder untouched")
}

// assertCorpusPatch compares the observed diff with the fixture, rewriting the fixture
// instead when -update is given.
func assertCorpusPatch(t *testing.T, path, got string) {
	t.Helper()
	if *updateLayoutCorpus {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o600))
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // a fixture path built from the corpus table
	require.NoError(t, err)
	require.Equal(t, string(want), got,
		"the write path and %s disagree; re-run with -update if the new diff is the intended one", path)
}

// readCorpusGitTarget decodes one scenario's GitTarget through the harness-local struct.
func readCorpusGitTarget(t *testing.T, path string) corpusGitTarget {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a fixture path built from the corpus table
	require.NoError(t, err)
	var target corpusGitTarget
	require.NoError(t, yaml.Unmarshal(raw, &target), "parsing %s", path)
	require.NotEmpty(t, target.Spec.Path, "%s: spec.path is what the corpus writes into", path)
	return target
}

// readCorpusInput decodes the live object a scenario receives. It is deliberately the
// object as the API server serves it — uid, resourceVersion, managedFields and all —
// because the difference between it and the expected patch IS the sanitization assertion.
func readCorpusInput(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a fixture path built from the corpus table
	require.NoError(t, err)
	obj := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal(raw, &obj.Object), "parsing %s", path)
	return obj
}

// corpusEvent builds the write event for a scenario's live object.
func corpusEvent(t *testing.T, obj *unstructured.Unstructured, target corpusGitTarget) Event {
	t.Helper()
	gvk := obj.GroupVersionKind()
	entry, ok := corpusTypes[gvk]
	require.True(t, ok, "corpusTypes has no entry for %s; add it beside the type it serves", gvk)
	return Event{
		Object: obj,
		Identifier: types.NewResourceIdentifier(
			gvk.Group, gvk.Version, entry.Resource, obj.GetNamespace(), obj.GetName()),
		Operation:          "CREATE",
		Path:               target.Spec.Path,
		GitTargetName:      target.Metadata.Name,
		GitTargetNamespace: target.Metadata.Namespace,
	}
}

// seedCorpusWorktree materialises a scenario's `repository/` into a fresh worktree and
// commits it, returning the worktree and the commit the diff is taken against.
//
// `repository/` is always rooted at the REPOSITORY root, never at spec.path, so a fixture
// shows where the target sits as well as what it holds. Shapes 6 to 8 depend on that: their
// target is a leaf overlay whose base lives outside spec.path but inside the render scope.
func seedCorpusWorktree(t *testing.T, repositoryDir string) (*gogit.Worktree, *object.Commit) {
	t.Helper()
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()

	require.NoError(t, filepath.WalkDir(repositoryDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(repositoryDir, path)
		if relErr != nil {
			return relErr
		}
		body, readErr := os.ReadFile(path) //nolint:gosec // walking a fixture folder
		if readErr != nil {
			return readErr
		}
		seedFile(t, root, rel, string(body))
		return nil
	}), "seeding %s", repositoryDir)

	return worktree, commitCorpusWorktree(t, worktree, "seed the fixture repository")
}

func commitCorpusWorktree(t *testing.T, worktree *gogit.Worktree, message string) *object.Commit {
	t.Helper()
	require.NoError(t, worktree.AddWithOptions(&gogit.AddOptions{All: true}))
	hash, err := worktree.Commit(message, &gogit.CommitOptions{
		Author: corpusSignature(), Committer: corpusSignature(), AllowEmptyCommits: true,
	})
	require.NoError(t, err)
	repo, err := gogit.PlainOpen(worktree.Filesystem().Root())
	require.NoError(t, err)
	commit, err := repo.CommitObject(hash)
	require.NoError(t, err)
	return commit
}

// corpusDiff renders what the flush did to the worktree as a unified diff in the
// fixtures' format: no `index` lines, because a blob hash is noise a reader cannot check
// and a rename or a whitespace change would churn for no reason.
func corpusDiff(t *testing.T, worktree *gogit.Worktree, base *object.Commit) string {
	t.Helper()
	after := commitCorpusWorktree(t, worktree, "the flush under test")

	baseTree, err := base.Tree()
	require.NoError(t, err)
	afterTree, err := after.Tree()
	require.NoError(t, err)
	changes, err := object.DiffTree(baseTree, afterTree)
	require.NoError(t, err)
	sort.Sort(changes)

	patch, err := changes.Patch()
	require.NoError(t, err)
	var rendered strings.Builder
	require.NoError(t, diff.NewUnifiedEncoder(&rendered, diff.DefaultContextLines).Encode(patch))
	return stripIndexLines(rendered.String())
}

// stripIndexLines removes the `index <hash>..<hash>` header line from every file patch.
func stripIndexLines(patch string) string {
	lines := strings.Split(patch, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, "index ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func corpusSignature() *object.Signature {
	return &object.Signature{Name: "layout corpus", Email: "corpus@example.com"}
}

// corpusTypes is the corpus's type registry: every kind any scenario's input carries. It
// is written out rather than discovered so a new fixture that forgets its type fails
// loudly in corpusEvent instead of resolving to an empty resource.
var corpusTypes = map[schema.GroupVersionKind]schema.GroupVersionResource{
	{Group: "", Version: "v1", Kind: "ConfigMap"}: {
		Group: "", Version: "v1", Resource: "configmaps"},
	{Group: "apps", Version: "v1", Kind: "Deployment"}: {
		Group: "apps", Version: "v1", Resource: "deployments"},
	{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}: {
		Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"},
	{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "HelmRepository"}: {
		Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmrepositories"},
	{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}: {
		Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"},
	{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}: {
		Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}: {
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
}

// corpusMapper serves every type in corpusTypes as followable, which is what namespace
// context resolution needs to run at all.
func corpusMapper() typeset.Lookup {
	entries := make([]typeset.Entry, 0, len(corpusTypes))
	for gvk, gvr := range corpusTypes {
		entries = append(entries, typeset.Entry{
			GVK:        gvk,
			GVR:        gvr,
			Namespaced: gvk.Kind != "ClusterRole",
			Allowed:    true,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].GVK.String() < entries[j].GVK.String() })
	return typeset.NewSnapshotRegistry(typeset.Snapshot{Entries: entries})
}

// TestLayoutCorpus_ConfigParsesAgainstTheRealAPI is the check the harness-local struct
// buys. corpusGitTarget exists because the examples name fields the API does not have
// yet; the risk that creates is the opposite one — a field the examples and the API BOTH
// have, spelled differently — so every shipped field a scenario config sets is decoded
// into the real v1alpha3.GitTarget too, and must survive the round trip.
//
// PR 2 deletes corpusGitTarget and this test with it: once serializeNamespace and
// useKustomize are real fields, the scenarios decode into v1alpha3.GitTarget directly and
// there is nothing left to keep honest.
func TestLayoutCorpus_ConfigParsesAgainstTheRealAPI(t *testing.T) {
	for _, sc := range layoutCorpus() {
		t.Run(sc.name(), func(t *testing.T) {
			path := filepath.Join(layoutCorpusRoot, sc.dir, "config", sc.configFile())
			harness := readCorpusGitTarget(t, path)

			raw, err := os.ReadFile(path) //nolint:gosec // a fixture path built from the corpus table
			require.NoError(t, err)
			// The unbuilt fields are stripped, not tolerated: decoding them into the real
			// type is exactly the thing that must start working in PR 2, and letting the
			// decoder ignore them here would hide the day it does.
			var real v1alpha3.GitTarget
			require.NoError(t, yaml.Unmarshal(withoutUnbuiltFields(t, raw), &real), "parsing %s", path)

			require.Equal(t, harness.Spec.Path, real.Spec.Path, "spec.path")
			require.Equal(t, harness.Metadata.Name, real.Name, "metadata.name")
			require.Equal(t, harness.Metadata.Namespace, real.Namespace, "metadata.namespace")
			require.Equal(t, harness.Spec.Branch, real.Spec.Branch, "spec.branch")
			if policy := harness.policy(); policy != nil {
				require.NotNil(t, real.Spec.Placement, "spec.placement")
				require.Equal(t, policy.ByType, real.Spec.Placement.ByType, "spec.placement.byType")
				require.Equal(t, policy.Default, real.Spec.Placement.Default, "spec.placement.default")
			}
		})
	}
}

// withoutUnbuiltFields removes the fields PR 2 introduces from a scenario config, so what
// is left is the API as it stands today. It is a line filter rather than a re-marshal
// because the configs are commented documents and the comments are half of what they say.
func withoutUnbuiltFields(t *testing.T, raw []byte) []byte {
	t.Helper()
	unbuilt := []string{"serializeNamespace:", "useKustomize:", "suspend:"}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if slicesContainsPrefix(trimmed, unbuilt) {
			continue
		}
		kept = append(kept, line)
	}
	return []byte(strings.Join(kept, "\n"))
}

func slicesContainsPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// TestLayoutCorpus_EveryFixtureFolderIsExecuted closes the corpus over the filesystem: a
// scenario folder that nothing in layoutCorpus() names is a fixture nobody runs, which is
// the state this whole file exists to leave behind.
func TestLayoutCorpus_EveryFixtureFolderIsExecuted(t *testing.T) {
	executed := map[string]bool{}
	for _, sc := range layoutCorpus() {
		executed[sc.dir] = true
	}
	for _, parent := range []string{"shapes", "specific-examples"} {
		entries, err := os.ReadDir(filepath.Join(layoutCorpusRoot, parent))
		require.NoError(t, err)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := parent + "/" + entry.Name()
			if _, err := os.Stat(filepath.Join(layoutCorpusRoot, dir, "input")); os.IsNotExist(err) {
				continue // a folder with no input/ illustrates prerequisites, not a write
			}
			require.True(t, executed[dir],
				fmt.Sprintf("%s has an input/ but no scenario in layoutCorpus() runs it", dir))
		}
	}
}
