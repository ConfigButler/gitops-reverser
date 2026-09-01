// SPDX-License-Identifier: Apache-2.0

package git

import (
	"flag"
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
	"github.com/ConfigButler/gitops-reverser/internal/layoutfixture"
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
//   - A scenario describing behavior that is not built yet is written NOW and skipped, naming the
//     track that unskips it. The corpus is the definition of done for that track: PR 2 is
//     finished when every skip naming PR 2 is gone. Not every skip is PR 2's — shape 8's
//     `images:` authoring belongs to track C and outlives it — so the rule is deliberately "its
//     own skips" rather than "the last skip".
//   - `config/gittarget.yaml` decodes into v1alpha3.GitTarget, the REAL type. It parsed into a
//     harness-local struct for as long as the examples named fields the API did not have, and
//     deleting that struct was PR 2's own definition of done: the worked examples and the shipped
//     API are now the same API, and a field either exists or the corpus stops decoding.
//   - Refusals are fixtures too. A scenario set where every write succeeds is
//     advertising rather than specification, so the refusal halves assert an
//     `expected-status.yaml` instead of a patch.
//
// Run with -update to rewrite the expected patches from the observed diff.

var updateLayoutCorpus = flag.Bool("update", false,
	"rewrite docs/layout expected-*.patch fixtures from the observed diff")

// layoutCorpusRoot is docs/layout/ as reached from this package's directory. The fixtures are read
// in place rather than copied into testdata/: a copy would drift from the documents it
// illustrates, and the drift would be invisible in review.
const layoutCorpusRoot = layoutfixture.Root

// corpusNamespaces projects a scenario onto the namespace policy the write path takes: the
// target's own spec.serializeNamespace, and the source namespaces the fixture's WatchRules bring
// to it.
func corpusNamespaces(target v1alpha3.GitTarget, sources []string, wildcard bool) namespacePolicy {
	return namespacePolicyFor(target.Spec, sources, wildcard)
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
	// patch names the expected diff under the fixture folder. Empty means the scenario asserts a
	// refusal instead, through status.
	patch string
	// status names the expected-*-status.yaml a refusal scenario asserts, instead of a patch.
	// The harness reads its GitPathAccepted condition WHOLE — status, reason and message — and
	// requires the flush to refuse with exactly that: the status is the refusal, the reason is
	// what manifestanalyzer.GitPathRefusalReason maps the refusal's issue kinds to, and the
	// message is the writer's own text. Asserting all three is what keeps the fixture a
	// specification rather than prose; asserting only the message let the reason drift.
	//
	// LayoutResolved and Stalled in the same file are the CONTROLLER's projection of this
	// refusal, which no write-path test can produce. They are asserted against the same fixture
	// by internal/controller (TestPublishLayout_AmbiguousMatchesTheCorpusFixture and
	// TestGitTargetReadiness_StalledFollowsGitPathAccepted), so the whole file is covered even
	// though no single test covers all of it.
	status string
	// emptyRepository seeds NOTHING, for a scenario whose whole subject is a folder that does not
	// exist yet. It is a flag rather than an empty `repository-empty/` fixture directory because
	// Git cannot hold an empty directory, and a .gitkeep inside one would show up in the diff the
	// scenario asserts.
	emptyRepository bool
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

// name is the subtest name: the folder plus what the scenario expects. It is keyed on the
// expectation rather than on the config, because two scenarios in one folder can share a config
// and differ only in their input — shape 8's image bump and env change do — and naming those by
// config produces "gittarget-prod" twice.
func (s corpusScenario) name() string {
	expectation := s.patch
	if expectation == "" {
		expectation = s.status
	}
	expectation = strings.TrimPrefix(expectation, "expected-")
	expectation = strings.TrimSuffix(strings.TrimSuffix(expectation, ".patch"), ".yaml")
	return s.dir + "/" + expectation
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
		},
		{
			// The fence around "one namespace": an explicit serializeNamespace: false admits
			// exactly one source namespace, and the second is refused because the two documents
			// would be indistinguishable rather than merely colliding.
			dir:    "shapes/2-flat-namespace-free",
			config: "gittarget-second-namespace.yaml",
			input:  "checkout-config.yaml",
			status: "expected-second-namespace-status.yaml",
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
		},
		{
			dir:   "shapes/5-kustomize-single-folder",
			input: "checkout-config.yaml",
			patch: "expected-checkout-config.patch",
		},
		{
			dir: "shapes/5-kustomize-single-folder",
			// The same folder before it exists. `repository/` is shape 5 ALREADY adopted, which is
			// the other scenario in this folder; this one seeds nothing, because "there is nothing
			// to infer from" is the whole subject.
			config:          "gittarget-empty-folder.yaml",
			input:           "checkout-config.yaml",
			patch:           "expected-empty-folder-first-write.patch",
			emptyRepository: true,
		},
		{
			dir:    "shapes/6-kustomize-base-and-overlays",
			config: "gittarget-prod.yaml",
			input:  "checkout-config.yaml",
			patch:  "expected-checkout-config.patch",
		},
		{
			// The one refusal in the corpus that asserts a rule this PR ships: a target at the
			// app root covers four render roots, so a new document has no single one to be
			// placed into. expected-app-root-status.yaml is the pair of conditions a user reads.
			dir:    "shapes/6-kustomize-base-and-overlays",
			config: "gittarget-app-root.yaml",
			input:  "checkout-config.yaml",
			status: "expected-app-root-status.yaml",
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
			dir:    "shapes/8-base-owned-field-edit",
			config: "gittarget-prod.yaml",
			input:  "deployment-env-changed.yaml",
			status: "expected-env-change-status.yaml",
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

	worktree, seeded := seedCorpusWorktree(t, folder, sc)
	event := corpusEvent(t, obj, target)

	sources, wildcard := readCorpusSourceNamespaces(t, folder, sc.configFile(), target)

	worker := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: corpusMapper()}
	_, err := worker.flushEventsToWorktree(
		t.Context(), worktree, sanitizePath(target.Spec.Path),
		[]Event{event}, resolvePlacementPolicy(target.Spec.Placement),
		corpusNamespaces(target, sources, wildcard), v1alpha3.PruneOnEvent)

	if sc.patch == "" {
		requireCorpusRefusal(t, err, filepath.Join(folder, sc.status), worktree, seeded)
		return
	}
	require.NoError(t, err, "%s: the scenario expects a commit, not a refusal", sc.name())

	got := corpusDiff(t, worktree, seeded)
	assertCorpusPatch(t, filepath.Join(folder, sc.patch), got)
}

// requireCorpusRefusal asserts a scenario that must not write: the flush refuses with the message
// the fixture's GitPathAccepted condition carries, and the worktree comes back exactly as it was
// seeded. The second half is not a formality — a refusal that leaves bytes behind is a partial
// commit, which is the failure mode the write-plan preconditions exist to prevent.
func requireCorpusRefusal(
	t *testing.T,
	err error,
	statusPath string,
	worktree *gogit.Worktree,
	seeded *object.Commit,
) {
	t.Helper()
	require.Error(t, err, "the scenario expects a refusal")

	want, err2 := layoutfixture.ReadCondition(statusPath, "GitPathAccepted")
	require.NoError(t, err2)
	require.Equal(t, "False", want.Status, "%s: a refusal is GitPathAccepted=False", statusPath)

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused,
		"the refusal must be an acceptance refusal, or it reaches status as an unexplained write fault")
	require.Equal(t, want.Reason, manifestanalyzer.GitPathRefusalReason(refused),
		"the reason this refusal publishes and the reason %s claims disagree", statusPath)
	require.Contains(t, refused.Error(), want.Message,
		"the refusal and %s disagree", statusPath)

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
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), got,
		"the write path and %s disagree; re-run with -update if the new diff is the intended one", path)
}

// readCorpusGitTarget decodes one scenario's GitTarget. It is the SHIPPED type: a config naming a
// field the API does not have fails here, which is the check the harness-local struct used to buy
// with a whole second parser and an assertion test beside it.
func readCorpusGitTarget(t *testing.T, path string) v1alpha3.GitTarget {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var target v1alpha3.GitTarget
	require.NoError(t, yaml.UnmarshalStrict(raw, &target), "parsing %s", path)
	require.NotEmpty(t, target.Spec.Path, "%s: spec.path is what the corpus writes into", path)
	return target
}

// readCorpusSourceNamespaces derives the source namespaces a scenario's target is reached by, from
// the WatchRules the fixture folder holds — the same set resolveSourceNamespaces computes from the
// cluster: the target's own namespace plus every explicit rules[].sourceNamespace.
//
// Which rules belong to a scenario is decided by the config's own name, because a folder can hold
// two configs that differ only in the rules pointing at them: shape 2's gittarget.yaml and
// gittarget-second-namespace.yaml are the same target with the same flag, and the ONLY difference
// between the write and the refusal is that a second WatchRule exists. So a config
// `gittarget[-<variant>].yaml` is served by `watchrule.yaml` plus `watchrule-<variant>.yaml`,
// whichever of the two the folder has, and every rule that matches must name the target — a
// fixture whose rules point somewhere else is a fixture that is not saying what it looks like it
// says.
func readCorpusSourceNamespaces(
	t *testing.T,
	folder, configFile string,
	target v1alpha3.GitTarget,
) ([]string, bool) {
	t.Helper()
	variant := strings.TrimSuffix(strings.TrimPrefix(configFile, "gittarget"), ".yaml")

	seen := map[string]struct{}{target.Namespace: {}}
	wildcard := false
	for _, name := range []string{"watchrule.yaml", "watchrule" + variant + ".yaml"} {
		path := filepath.Join(folder, "config", name)
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		var rule v1alpha3.WatchRule
		require.NoError(t, yaml.Unmarshal(raw, &rule), "parsing %s", path)
		require.Equal(t, target.Name, rule.Spec.TargetRef.Name,
			"%s points at a different GitTarget than the scenario's config", path)
		for _, item := range rule.Spec.Rules {
			if item.IsSourceNamespaceWildcard() {
				wildcard = true
				continue
			}
			seen[item.EffectiveSourceNamespace(rule.Namespace)] = struct{}{}
		}
	}

	namespaces := make([]string, 0, len(seen))
	for ns := range seen {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	return namespaces, wildcard
}

// readCorpusInput decodes the live object a scenario receives. It is deliberately the
// object as the API server serves it — uid, resourceVersion, managedFields and all —
// because the difference between it and the expected patch IS the sanitization assertion.
func readCorpusInput(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	obj := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal(raw, &obj.Object), "parsing %s", path)
	return obj
}

// corpusEvent builds the write event for a scenario's live object.
func corpusEvent(t *testing.T, obj *unstructured.Unstructured, target v1alpha3.GitTarget) Event {
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
		GitTargetName:      target.Name,
		GitTargetNamespace: target.Namespace,
	}
}

// seedCorpusWorktree materialises a scenario's `repository/` into a fresh worktree and
// commits it, returning the worktree and the commit the diff is taken against.
//
// `repository/` is always rooted at the REPOSITORY root, never at spec.path, so a fixture
// shows where the target sits as well as what it holds. Shapes 6 to 8 depend on that: their
// target is a leaf overlay whose base lives outside spec.path but inside the render scope.
func seedCorpusWorktree(t *testing.T, folder string, sc corpusScenario) (*gogit.Worktree, *object.Commit) {
	t.Helper()
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	if sc.emptyRepository {
		return worktree, commitCorpusWorktree(t, worktree, "seed an empty repository")
	}
	repositoryDir := filepath.Join(folder, "repository")

	require.NoError(t, filepath.WalkDir(repositoryDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(repositoryDir, path)
		if relErr != nil {
			return relErr
		}
		body, readErr := os.ReadFile(path)
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
				"%s has an input/ but no scenario in layoutCorpus() runs it", dir)
		}
	}
}
