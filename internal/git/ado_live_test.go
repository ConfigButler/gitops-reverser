// SPDX-License-Identifier: Apache-2.0

package git

// An opt-in test against a real Azure DevOps repository.
//
// The simulator in ado_multiack_test.go reproduces ADO's multi_ack rule faithfully enough to gate CI,
// but it is still a local stand-in: it asserts what we believe ADO does. This one asserts what ADO
// actually does, and it is the only place that belief gets checked. It is skipped unless the
// environment supplies a repository, so it never runs in CI.
//
//	export E2E_ADO_REPO_URL='https://dev.azure.com/<org>/<project>/_git/<repo>'
//	export E2E_ADO_PAT='<personal access token>'   # scope: Code (read & write)
//	go test ./internal/git/ -run TestADOLive -v
//
// E2E_ADO_USERNAME is optional; ADO ignores the username when a PAT is the password, and the default
// (empty) is the form ADO documents.
//
// It writes freely to the repository, including the default branch, and does not clean up. Point it at
// a scratch repository.
//
// Beyond "does ADO work", the substance here is the branch-resolution order SmartFetch promises:
// prefer the target branch, always fetch the default branch as a safety net, and report nothing for an
// empty repository. That ordering is easy to get subtly wrong and cheap to assert, so
// TestADOLive_BranchResolutionOrder walks it against the real remote.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/object"
	gogithttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adoLiveTarget is the repository and credential under test.
type adoLiveTarget struct {
	URL  string
	Auth []gitclient.Option
}

func adoLiveFromEnv(tb testing.TB) adoLiveTarget {
	tb.Helper()

	url := os.Getenv("E2E_ADO_REPO_URL")
	pat := os.Getenv("E2E_ADO_PAT")
	if url == "" || pat == "" {
		tb.Skip("set E2E_ADO_REPO_URL and E2E_ADO_PAT to run the live Azure DevOps test")
	}

	// Empty username with the PAT as password is the form ADO documents.
	cred := Credential{Basic: &gogithttp.BasicAuth{
		Username: os.Getenv("E2E_ADO_USERNAME"),
		Password: pat,
	}}

	return adoLiveTarget{URL: url, Auth: cred.Options()}
}

// TestADOLive_CheckRepo is the cheapest real-ADO assertion: connectivity and metadata over the ref
// advertisement, which needs no multi_ack. If this passes while the fetch test fails, the capability
// matrix is right and only negotiation is affected.
//
// An empty repository is a pass. Reaching the advertisement is what is being checked, and a freshly
// created ADO repository has no commits.
func TestADOLive_CheckRepo(t *testing.T) {
	target := adoLiveFromEnv(t)

	info, err := CheckRepo(context.Background(), target.URL, target.Auth)
	require.NoError(t, err, "CheckRepo reads only the advertisement and must not need multi_ack")

	if info.DefaultBranch == nil {
		t.Log("repository is empty: the advertisement was served, which is what this test checks")
		assert.Zero(t, info.RemoteBranchCount)
		return
	}

	t.Logf("default branch %q sha=%s unborn=%t, %d remote branches",
		info.DefaultBranch.ShortName, info.DefaultBranch.Sha,
		info.DefaultBranch.Unborn, info.RemoteBranchCount)
	assert.Positive(t, info.RemoteBranchCount)
}

// TestADOLive_BranchResolutionOrder walks the branch-resolution contract against the real remote, in
// the order the code implements it, and is the test to read when that order changes.
//
// The fetch in phase 4 is also the one that matters for ADO: by then the local store has history, so
// the request carries have lines and a real negotiation happens. That is the exact request go-git v5
// could not make — it fails with HTTP 400 "TF401041: Clients must support multi-ack" — and the whole
// reason for the v6 migration.
func TestADOLive_BranchResolutionOrder(t *testing.T) {
	target := adoLiveFromEnv(t)
	ctx := context.Background()

	// --- Phase 0: an empty repository reports no branch at all -----------------------------------
	info, err := CheckRepo(ctx, target.URL, target.Auth)
	require.NoError(t, err)

	if info.DefaultBranch == nil {
		t.Log("phase 0: repository is empty — CheckRepo reports no default branch")
		assert.Zero(t, info.RemoteBranchCount)

		empty := filepath.Join(t.TempDir(), "empty-probe")
		probe := adoLiveRepo(t, empty, target.URL)
		resolved, ferr := SmartFetch(ctx, probe, plumbing.NewBranchReferenceName("main"), target.Auth)
		require.NoError(t, ferr, "fetching an empty remote is a valid state, not an error")
		assert.Empty(t, resolved, "an empty remote resolves to no branch")

		adoLiveSeed(t, target, "main")
	}

	// --- Phase 1: the default branch is reported, with a hash, and is not unborn ------------------
	info, err = CheckRepo(ctx, target.URL, target.Auth)
	require.NoError(t, err)
	require.NotNil(t, info.DefaultBranch, "a seeded repository must report a default branch")

	defaultBranch := info.DefaultBranch.ShortName
	t.Logf("phase 1: default branch is %q at %s (unborn=%t)",
		defaultBranch, info.DefaultBranch.Sha, info.DefaultBranch.Unborn)
	assert.False(t, info.DefaultBranch.Unborn, "a branch with commits must not be reported unborn")
	assert.NotEmpty(t, info.DefaultBranch.Sha)

	// --- Phase 2: a target that does not exist falls back to the default -------------------------
	work := filepath.Join(t.TempDir(), "work")
	repo, err := PrepareBranchLive(ctx, t, target, work, defaultBranch)
	require.NoError(t, err)

	absent := plumbing.NewBranchReferenceName(liveBranchName("absent"))
	resolved, err := SmartFetch(ctx, repo, absent, target.Auth)
	require.NoError(t, err)
	assert.Equal(t, plumbing.NewBranchReferenceName(defaultBranch), resolved,
		"a target missing on the remote must fall back to the default branch")
	t.Logf("phase 2: target %q is absent, resolved to %q", absent.Short(), resolved.Short())

	requireRemoteTracking(t, repo, defaultBranch, "the default branch is always fetched as a safety net")

	// --- Phase 3: a target that does exist wins, and the default is still fetched ----------------
	feature := liveBranchName("feature")
	rootHash := adoLivePushBranch(ctx, t, target, repo, defaultBranch, feature, "phase 3: create the target branch")

	resolved, err = SmartFetch(ctx, repo, plumbing.NewBranchReferenceName(feature), target.Auth)
	require.NoError(t, err)
	assert.Equal(t, plumbing.NewBranchReferenceName(feature), resolved,
		"a target present on the remote must win over the default")
	t.Logf("phase 3: target %q exists, resolved to %q", feature, resolved.Short())

	requireRemoteTracking(t, repo, feature, "the target branch must be fetched")
	requireRemoteTracking(t, repo, defaultBranch, "the default branch is fetched even when the target wins")

	// --- Phase 4: the negotiating fetch -----------------------------------------------------------
	// Move the remote on from a second clone, so the fetch below has something to retrieve and the
	// local store already has history to advertise as haves.
	second := filepath.Join(t.TempDir(), "second")
	secondRepo, err := PrepareBranchLive(ctx, t, target, second, defaultBranch)
	require.NoError(t, err)
	advanced := adoLiveCommitAndPush(ctx, t, target, secondRepo, defaultBranch, rootHash,
		"phase 4: advance the default branch")

	resolved, err = SmartFetch(ctx, repo, plumbing.NewBranchReferenceName(defaultBranch), target.Auth)
	require.NoError(t, err,
		"an incremental fetch against ADO must succeed; go-git v5 fails here with HTTP 400 TF401041")
	assert.Equal(t, plumbing.NewBranchReferenceName(defaultBranch), resolved)

	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", defaultBranch), true)
	require.NoError(t, err)
	assert.Equal(t, advanced, ref.Hash(),
		"the negotiating fetch must have retrieved the commit pushed from elsewhere")
	t.Logf("phase 4: negotiated fetch advanced origin/%s to %s", defaultBranch, ref.Hash())
}

// liveBranchName returns a branch name unlikely to collide with anything in the repository.
func liveBranchName(kind string) string {
	return fmt.Sprintf("reverser-live-%s-%d", kind, os.Getpid())
}

// adoLiveRepo initialises an empty local repository wired to the remote.
func adoLiveRepo(tb testing.TB, path, url string) *git.Repository {
	tb.Helper()

	repo, err := git.PlainInit(path, false)
	require.NoError(tb, err)
	require.NoError(tb, PinExplicitSigningPolicy(repo))
	_, err = repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}})
	require.NoError(tb, err)

	return repo
}

// PrepareBranchLive clones through the production PrepareBranch path and reopens the result.
func PrepareBranchLive(
	ctx context.Context, tb testing.TB, target adoLiveTarget, path, branch string,
) (*git.Repository, error) {
	tb.Helper()

	if _, err := PrepareBranch(ctx, target.URL, path, branch, target.Auth); err != nil {
		return nil, err
	}
	return git.PlainOpen(path)
}

// adoLiveSeed gives an empty repository its first commit on branch.
func adoLiveSeed(tb testing.TB, target adoLiveTarget, branch string) {
	tb.Helper()
	ctx := context.Background()

	dir := filepath.Join(tb.TempDir(), "seed")
	repo := adoLiveRepo(tb, dir, target.URL)
	require.NoError(tb, setHead(repo, branch))

	adoLiveWriteAndCommit(tb, repo, dir, "README.md",
		"# scratch repository for the gitops-reverser live Azure DevOps test\n",
		"test: seed the live Azure DevOps scratch repository")

	require.NoError(tb,
		PushAtomic(ctx, repo, plumbing.ZeroHash, plumbing.NewBranchReferenceName(branch), target.Auth),
		"creating the first branch on an empty ADO repository must succeed")
	tb.Logf("seeded %q with a first commit", branch)
}

// adoLivePushBranch branches off the default branch and pushes the new branch, returning the hash the
// branch was based on -- which is the root hash the push compared against.
func adoLivePushBranch(
	ctx context.Context, tb testing.TB, target adoLiveTarget,
	repo *git.Repository, defaultBranch, newBranch, message string,
) plumbing.Hash {
	tb.Helper()

	rootHash, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", defaultBranch), true)
	require.NoError(tb, err)

	worktree, err := repo.Worktree()
	require.NoError(tb, err)
	require.NoError(tb, worktree.Checkout(&git.CheckoutOptions{
		Hash:   rootHash.Hash(),
		Branch: plumbing.NewBranchReferenceName(newBranch),
		Create: true,
		Force:  true,
	}))

	root, err := repo.Worktree()
	require.NoError(tb, err)
	adoLiveWriteAndCommit(tb, repo, root.Filesystem().Root(),
		"reverser-live-test.yaml", fmt.Sprintf("# %s at %s\n", message, time.Now().UTC()), message)

	require.NoError(tb,
		PushAtomic(ctx, repo, rootHash.Hash(), plumbing.NewBranchReferenceName(defaultBranch), target.Auth),
		"receive-pack has no multi_ack, so the push must succeed")
	tb.Logf("pushed branch %q", newBranch)

	return rootHash.Hash()
}

// adoLiveCommitAndPush commits on the checked-out default branch and pushes it, returning the new hash.
func adoLiveCommitAndPush(
	ctx context.Context, tb testing.TB, target adoLiveTarget,
	repo *git.Repository, defaultBranch string, _ plumbing.Hash, message string,
) plumbing.Hash {
	tb.Helper()

	rootRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", defaultBranch), true)
	require.NoError(tb, err)

	worktree, err := repo.Worktree()
	require.NoError(tb, err)
	require.NoError(tb, worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(defaultBranch),
		Force:  true,
	}))

	newHash := adoLiveWriteAndCommit(tb, repo, worktree.Filesystem().Root(),
		"reverser-live-test.yaml", fmt.Sprintf("# %s at %s\n", message, time.Now().UTC()), message)

	require.NoError(tb,
		PushAtomic(ctx, repo, rootRef.Hash(), plumbing.NewBranchReferenceName(defaultBranch), target.Auth))
	tb.Logf("advanced %q to %s", defaultBranch, newHash)

	return newHash
}

// adoLiveWriteAndCommit writes a file and commits it, returning the commit hash.
func adoLiveWriteAndCommit(tb testing.TB, repo *git.Repository, dir, name, content, message string) plumbing.Hash {
	tb.Helper()

	worktree, err := repo.Worktree()
	require.NoError(tb, err)

	require.NoError(tb, os.WriteFile(filepath.Join(dir, name), []byte(content), 0600))
	_, err = worktree.Add(name)
	require.NoError(tb, err)

	hash, err := worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "reverser-live-test", Email: "noreply@example.com", When: time.Now()},
	})
	require.NoError(tb, err)

	return hash
}

// requireRemoteTracking asserts a remote-tracking ref exists locally after a fetch.
func requireRemoteTracking(tb testing.TB, repo *git.Repository, branch, why string) {
	tb.Helper()

	_, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	require.NoError(tb, err, why)
}
