// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gossh "golang.org/x/crypto/ssh"
)

// A stalled Git server must hand control back to the branch worker. See network_bound.go for the
// measurement this pins: HTTP honors a context only through go-git's context-taking API, and SSH
// does not honor one at all past the dial.

const (
	stallBound = 300 * time.Millisecond
	// stallGuard is how long a call may take before the test calls it hung. It is generous: the
	// assertion is "returns at all", and the deadline is checked separately through the error.
	stallGuard = 5 * time.Second
)

// shortenGitBounds sets both bounds for one test.
func shortenGitBounds(t *testing.T, call, publish time.Duration) {
	t.Helper()
	oldCall, oldPublish := gitCallTimeout, gitPublishTimeout
	gitCallTimeout, gitPublishTimeout = call, publish
	t.Cleanup(func() { gitCallTimeout, gitPublishTimeout = oldCall, oldPublish })
}

// requireReturns runs op and fails the test if it does not come back within stallGuard.
func requireReturns(t *testing.T, name string, op func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- op() }()
	select {
	case err := <-done:
		return err
	case <-time.After(stallGuard):
		t.Fatalf("%s did not return within %v against a stalled server", name, stallGuard)
		return nil
	}
}

// holdUntilClientLeaves stalls a request: it answers nothing until the client gives up. It reads
// the body first, because the server only notices a client hanging up once the body is consumed.
func holdUntilClientLeaves(_ http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
}

// stallPhase installs a hook on sim that holds every request matching match.
func stallPhase(sim *adoSimulator, match func(*http.Request) bool) {
	around := func(w http.ResponseWriter, r *http.Request, backend http.Handler) {
		if match(r) {
			holdUntilClientLeaves(w, r)
			return
		}
		backend.ServeHTTP(w, r)
	}
	sim.around.Store(&around)
}

func isInfoRefs(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/info/refs") }
func isUploadPack(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack")
}
func isReceivePack(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack")
}

// Before step 6 SmartFetchFrom, CheckRepo and the advertisement called go-git's context-free List
// and Fetch, and each of these hung past its context.
func TestGitNetworkCalls_HTTPStallsReturnAtTheCallBound(t *testing.T) {
	shortenGitBounds(t, stallBound, time.Minute)
	projectRoot, repoDir := newADORepo(t)
	sim := startRealGitServer(t, projectRoot, repoDir)
	clone, err := git.PlainClone(t.TempDir(), &git.CloneOptions{URL: sim.RepoURL})
	require.NoError(t, err)
	main := plumbing.NewBranchReferenceName("main")
	head, err := clone.Head()
	require.NoError(t, err)
	ctx := context.Background()

	calls := []struct {
		name  string
		stall func(*http.Request) bool
		call  func() error
	}{
		{"SmartFetchFrom, advertisement", isInfoRefs, func() error {
			_, err := SmartFetchFrom(ctx, clone, main, "", nil)
			return err
		}},
		{"SmartFetchFrom, transfer", isUploadPack, func() error {
			_, err := SmartFetchFrom(ctx, clone, main, "", nil)
			return err
		}},
		{"CheckRepo", isInfoRefs, func() error {
			_, err := CheckRepo(ctx, sim.RepoURL, nil)
			return err
		}},
		{"advertiseRemoteBranch", isInfoRefs, func() error {
			_, err := advertiseRemoteBranch(ctx, sim.RepoURL, main, "", nil)
			return err
		}},
		{"PushAtomic, advertisement", isInfoRefs, func() error {
			_, err := PushAtomic(ctx, clone, head.Hash(), main, nil)
			return err
		}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			// The transfer only stalls when there is something to transfer.
			if c.name == "SmartFetchFrom, transfer" {
				simulateClientCommitOnDisk(t, repoDir, "main", "moved-"+strconv.Itoa(time.Now().Nanosecond()), "x\n")
			}
			stallPhase(sim, c.stall)
			t.Cleanup(func() { sim.around.Store(nil) })
			err := requireReturns(t, c.name, c.call)
			require.Error(t, err)
			require.ErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}

// insecureSSHAuth is a password auth that trusts any host key, with the host key algorithms set so
// go-git does not consult an on-disk known_hosts.
type insecureSSHAuth struct{}

func (insecureSSHAuth) ClientConfig(context.Context, *transport.Request) (*gossh.ClientConfig, error) {
	return &gossh.ClientConfig{
		User:              "git",
		Auth:              []gossh.AuthMethod{gossh.Password("x")},
		HostKeyCallback:   gossh.InsecureIgnoreHostKey(),
		HostKeyAlgorithms: []string{gossh.KeyAlgoED25519},
	}, nil
}

// silentTCP accepts connections and never writes a byte: no SSH banner.
func silentTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	return ln.Addr().String()
}

// silentSSH completes the SSH handshake and accepts the exec request, then sends nothing: the git
// protocol stalls.
func silentSSH(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := gossh.NewSignerFromKey(priv)
	require.NoError(t, err)
	cfg := &gossh.ServerConfig{PasswordCallback: func(gossh.ConnMetadata, []byte) (*gossh.Permissions, error) {
		return &gossh.Permissions{}, nil
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
			go serveSilentSSH(c, cfg)
		}
	}()
	return ln.Addr().String()
}

func serveSilentSSH(c net.Conn, cfg *gossh.ServerConfig) {
	_, chans, reqs, err := gossh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go gossh.DiscardRequests(reqs)
	for nc := range chans {
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer func() { _ = ch.Close() }()
			for r := range chReqs {
				_ = r.Reply(true, nil)
			}
		}()
	}
}

// sshCloneOf is a local repository whose origin is url, so the fetch and push paths have a
// checkout to work from without a server that can serve one.
func sshCloneOf(t *testing.T, url string) *git.Repository {
	t.Helper()
	repo, err := git.PlainInit(t.TempDir(), false)
	require.NoError(t, err)
	_, err = repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}})
	require.NoError(t, err)
	return repo
}

// go-git's SSH transport uses the context only to dial, so before step 6 every one of these hung
// with a context that had long expired. The connection is now closed when the call's bound ends.
func TestGitNetworkCalls_SSHStallsReturnAtTheCallBound(t *testing.T) {
	shortenGitBounds(t, stallBound, time.Minute)
	auth := []gitclient.Option{gitclient.WithSSHAuth(insecureSSHAuth{})}
	main := plumbing.NewBranchReferenceName("main")
	ctx := context.Background()

	for _, server := range []struct {
		name string
		addr string
	}{
		{"no SSH banner", silentTCP(t)},
		{"SSH handshake done, git silent", silentSSH(t)},
	} {
		url := "ssh://git@" + server.addr + "/repo.git"
		repo := sshCloneOf(t, url)
		for _, c := range []struct {
			name string
			call func() error
		}{
			{"CheckRepo", func() error { _, err := CheckRepo(ctx, url, auth); return err }},
			{"SmartFetchFrom", func() error { _, err := SmartFetchFrom(ctx, repo, main, "", auth); return err }},
			{"advertiseRemoteBranch", func() error {
				_, err := advertiseRemoteBranch(ctx, url, main, "", auth)
				return err
			}},
			{"PushAtomic", func() error {
				_, err := PushAtomic(ctx, repo, plumbing.ZeroHash, main, auth)
				return err
			}},
		} {
			t.Run(server.name+"/"+c.name, func(t *testing.T) {
				err := requireReturns(t, c.name, c.call)
				require.Error(t, err)
				require.ErrorIs(t, err, context.DeadlineExceeded)
			})
		}
	}
}

// Worker shutdown cancels the worker's context. A stalled SSH call used to ignore that too, so
// Stop, which waits for the loop, waited for the TCP connection to die on its own.
func TestGitNetworkCalls_ASSHStallEndsWhenTheWorkerIsCancelled(t *testing.T) {
	shortenGitBounds(t, time.Hour, time.Hour)
	auth := []gitclient.Option{gitclient.WithSSHAuth(insecureSSHAuth{})}
	url := "ssh://git@" + silentSSH(t) + "/repo.git"

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(stallBound, cancel)
	err := requireReturns(t, "CheckRepo", func() error {
		_, err := CheckRepo(ctx, url, auth)
		return err
	})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// A push whose reply is lost after the server applied it is an uncertain outcome. The cycle's
// probe finds the branch at the commits it sent, and settles the push as published instead of
// replaying them. Replaying would re-plan the same writes onto a tree that already holds them, and a
// save's empty commit would land on the remote twice.
func TestPushCycle_ALostReplyThatLandedIsPublishedNotReplayed(t *testing.T) {
	shortenGitBounds(t, stallBound, time.Minute)
	f := newLedgerFixture(t, "lost-reply", true)
	f.createLedgerTarget("apps", nil)

	record, err := f.worker.buildRequestRecordWrite(f.worker.ctx, &pendingCommitRequest{
		id:                 commitRequestID{Namespace: "default", Name: "save", UID: "save-uid"},
		author:             "alice",
		gitTargetName:      ledgerTargetName,
		gitTargetNamespace: "default",
		message:            "save",
	})
	require.NoError(t, err)
	require.NoError(t, f.worker.commitPendingWrites([]PendingWrite{*record}))
	local := workerHead(t, f.worker)
	before := countCommits(t, f.repoDir)

	var swallowed atomic.Int32
	around := func(w http.ResponseWriter, r *http.Request, backend http.Handler) {
		if isReceivePack(r) && swallowed.Add(1) == 1 {
			backend.ServeHTTP(httptest.NewRecorder(), r) // the server applies the push...
			holdUntilClientLeaves(w, r)                  // ...and the reply never arrives
			return
		}
		backend.ServeHTTP(w, r)
	}
	f.sim.around.Store(&around)

	err = requireReturns(t, "pushPendingCommits", func() error {
		return f.worker.pushPendingCommits([]PendingWrite{*record})
	})
	require.NoError(t, err, "the probe proves the push landed")
	assert.Equal(t, int32(1), swallowed.Load(), "nothing was pushed a second time")
	assert.Equal(t, local.String(), revParseMain(t, f.repoDir), "the remote is at the commit we sent")
	assert.Equal(t, before+1, countCommits(t, f.repoDir), "the save's empty commit landed exactly once")
}

// The cycle's deadline covers the whole cycle. With a per-call bound longer than the cycle's, a
// stalled upload still hands control back when the cycle's budget is spent, and the writes stay
// with the caller for the retry.
func TestPushCycle_OneDeadlineCoversTheWholeCycle(t *testing.T) {
	f := newLedgerFixture(t, "cycle-deadline", true)
	f.commit("cm-a")
	shortenGitBounds(t, time.Hour, stallBound)
	stallPhase(f.sim, isReceivePack)

	started := time.Now()
	err := requireReturns(t, "pushPendingCommits", func() error {
		return f.worker.pushPendingCommits(f.pending)
	})
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 2*time.Second, "returned at the cycle's budget, not a call's")
	assert.NotEqual(t, workerHead(t, f.worker).String(), revParseMain(t, f.repoDir), "nothing landed")
}

func workerHead(t *testing.T, w *BranchWorker) plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(w.repoPath())
	require.NoError(t, err)
	_, head, err := GetCurrentBranch(repo)
	require.NoError(t, err)
	return head
}

func countCommits(t *testing.T, repoDir string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", repoDir, "rev-list", "--count", "refs/heads/main").Output()
	require.NoError(t, err)
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	return n
}
