// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sshpkg "github.com/ConfigButler/gitops-reverser/internal/ssh"
)

// lsRemoteFields is the number of tab-separated fields in a git ls-remote line.
const lsRemoteFields = 2

// credentialPattern matches HTTP/HTTPS URLs that embed credentials as userinfo
// (https://user:password@host/...). Used to redact secrets from error messages.
var credentialPattern = regexp.MustCompile(`(?i)(https?://)[^:@\s]+:[^@\s]+@`)

// IsADOURL reports whether rawURL targets Azure DevOps. go-git v5 cannot negotiate
// the multi_ack capability that ADO requires; those repositories must use the system
// git fallback.
//
// Detection is based on the parsed hostname, not substring matching, to prevent
// crafted URLs (e.g. ext:: remote helpers) from hijacking the routing decision.
func IsADOURL(rawURL string) bool {
	host, ok := parseGitURLHost(rawURL)
	if !ok {
		return false
	}
	lower := strings.ToLower(host)
	return lower == "dev.azure.com" || lower == "ssh.dev.azure.com" ||
		strings.HasSuffix(lower, ".visualstudio.com")
}

// parseGitURLHost extracts the hostname from a Git URL and validates that the
// scheme is safe for subprocess execution. It handles both standard hierarchical
// URLs (https://, ssh://) and SCP-style shorthand ([user@]host:path).
// Returns ("", false) for URLs with dangerous schemes (ext::, file::) or that
// cannot be parsed.
func parseGitURLHost(rawURL string) (string, bool) {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		switch strings.ToLower(u.Scheme) {
		case "https", "http", "ssh", "git+ssh", "ssh+git", "git":
			return u.Hostname(), true
		}
		return "", false
	}
	// SCP-style: [user@]host:path — url.Parse yields no host.
	// Reject anything containing whitespace first.
	if strings.ContainsAny(rawURL, " \t\n\r") {
		return "", false
	}
	s := rawURL
	if at := strings.Index(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	if colon := strings.Index(s, ":"); colon > 0 {
		return s[:colon], true
	}
	return "", false
}

// systemGitEnv holds the environment variables needed to run git with the caller's
// credentials, and a cleanup function that removes any temp files it created.
type systemGitEnv struct {
	vars    []string
	cleanup func()
}

// newSystemGitEnv translates a go-git AuthMethod into environment variables consumed
// by a system git subprocess.
//
//   - *http.BasicAuth   → GIT_CONFIG_GLOBAL with http.extraHeader = Authorization: Basic <base64>
//   - *http.TokenAuth   → GIT_CONFIG_GLOBAL with http.extraHeader = Authorization: Bearer <token>
//   - *sshpkg.KeyAuth   → GIT_SSH_COMMAND with -i <keyfile> and optional known_hosts
//   - nil               → anonymous (no extra env)
//   - anything else     → error
//
// HTTP auth types write a 0600 temp gitconfig and set GIT_CONFIG_GLOBAL.
// Credentials never appear in process argv.
//
// The repoURL parameter is used to scope HTTP credential headers to the remote
// origin so they are never sent to any other host git contacts.
func newSystemGitEnv(repoURL string, auth transport.AuthMethod) (*systemGitEnv, error) {
	// Scope HTTP credential headers to the remote origin so they are never sent to
	// any other host git might contact (e.g. a redirect or a submodule remote).
	httpSection := "http"
	if u, err := url.Parse(repoURL); err == nil && u.Scheme != "" && u.Host != "" {
		httpSection = fmt.Sprintf("http %q", u.Scheme+"://"+u.Host+"/")
	}

	tmpDir, err := os.MkdirTemp("", "reverser-git-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir for git credentials: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	base := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"PATH=" + os.Getenv("PATH"),
	}

	switch a := auth.(type) {
	case *http.BasicAuth:
		encoded := base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))
		cfgPath := filepath.Join(tmpDir, "gitconfig")
		content := fmt.Sprintf("[%s]\n\textraHeader = Authorization: Basic %s\n", httpSection, encoded)
		if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
			cleanup()
			return nil, fmt.Errorf("write git credential config: %w", err)
		}
		return &systemGitEnv{
			vars:    append(base, "GIT_CONFIG_GLOBAL="+cfgPath),
			cleanup: cleanup,
		}, nil

	case *http.TokenAuth:
		if strings.ContainsAny(a.Token, "\n\r") {
			cleanup()
			return nil, errors.New("bearer token contains invalid characters (newline)")
		}
		cfgPath := filepath.Join(tmpDir, "gitconfig")
		content := fmt.Sprintf("[%s]\n\textraHeader = Authorization: Bearer %s\n", httpSection, a.Token)
		if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
			cleanup()
			return nil, fmt.Errorf("write git credential config: %w", err)
		}
		return &systemGitEnv{
			vars:    append(base, "GIT_CONFIG_GLOBAL="+cfgPath),
			cleanup: cleanup,
		}, nil

	case *sshpkg.KeyAuth:
		if len(a.PrivateKeyPEM) == 0 {
			cleanup()
			return nil, errors.New(
				"SSH key type is not supported by the system-git ADO fallback " +
					"(key re-serialisation failed); use RSA, ECDSA, or Ed25519",
			)
		}
		keyFile := filepath.Join(tmpDir, "id_key")
		if err := os.WriteFile(keyFile, a.PrivateKeyPEM, 0600); err != nil {
			cleanup()
			return nil, fmt.Errorf("write SSH private key: %w", err)
		}
		sshCmd := "ssh -i '" + keyFile + "' -o BatchMode=yes"
		if a.KnownHosts != "" {
			khFile := filepath.Join(tmpDir, "known_hosts")
			if err := os.WriteFile(khFile, []byte(a.KnownHosts), 0600); err != nil {
				cleanup()
				return nil, fmt.Errorf("write SSH known_hosts: %w", err)
			}
			sshCmd += " -o StrictHostKeyChecking=yes -o UserKnownHostsFile='" + khFile + "'"
		} else {
			// No known_hosts supplied: accept new host keys but reject changed ones.
			sshCmd += " -o StrictHostKeyChecking=accept-new"
		}
		return &systemGitEnv{
			vars:    append(base, "GIT_SSH_COMMAND="+sshCmd),
			cleanup: cleanup,
		}, nil

	case nil:
		return &systemGitEnv{vars: base, cleanup: cleanup}, nil

	default:
		cleanup()
		return nil, fmt.Errorf(
			"system git fallback for ADO does not support auth type %T; use an HTTPS or SSH credential",
			auth,
		)
	}
}

// run executes git with the configured environment. workDir is passed as -C when non-empty.
func (e *systemGitEnv) run(ctx context.Context, workDir string, args ...string) ([]byte, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, errors.New(
			"git binary not found in PATH; add git to the container image to enable the ADO fallback",
		)
	}

	fullArgs := args
	if workDir != "" {
		fullArgs = append([]string{"-C", workDir}, args...)
	}

	cmd := exec.CommandContext(ctx, gitBin, fullArgs...)
	cmd.Env = e.vars

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Redact any embedded credentials (https://user:PAT@host) before logging.
		safeArgs := make([]string, len(args))
		for i, a := range args {
			safeArgs[i] = credentialPattern.ReplaceAllString(a, "${1}<redacted>@")
		}
		safeStderr := credentialPattern.ReplaceAllString(stderr.String(), "${1}<redacted>@")
		return nil, fmt.Errorf("git %v: %w\nstderr: %s", safeArgs, err, safeStderr)
	}

	return stdout.Bytes(), nil
}

// remoteOriginURL returns the first URL configured for the "origin" remote, or an
// error if the remote does not exist or has no URLs (e.g. a hand-edited .git/config).
func remoteOriginURL(repo *gogit.Repository) (string, error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		return "", fmt.Errorf("get remote origin: %w", err)
	}
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return "", errors.New("remote origin has no URLs configured")
	}
	return urls[0], nil
}

// repoWorkDir returns the working-tree root of a filesystem-backed repository.
// Returns "" when the repository uses in-memory storage (tests).
func repoWorkDir(repo *gogit.Repository) string {
	fsStorage, ok := repo.Storer.(*filesystem.Storage)
	if !ok {
		return ""
	}
	type rooter interface{ Root() string }
	if r, ok := fsStorage.Filesystem().(rooter); ok {
		return filepath.Dir(r.Root())
	}
	return ""
}

// parseADOLSRemote parses `git ls-remote --symref` output and returns:
//   - refs: map of full-refname → SHA (e.g. "refs/heads/main" → "abc123")
//   - defaultBranch: the short branch name HEAD resolves to (empty if HEAD is missing/broken)
func parseADOLSRemote(out []byte) (map[string]string, string) {
	refs := make(map[string]string)
	var defaultBranch string

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "ref: ") {
			// symbolic ref line: "ref: refs/heads/main\tHEAD"
			parts := strings.SplitN(line, "\t", lsRemoteFields)
			if len(parts) == lsRemoteFields && parts[1] == "HEAD" {
				target := strings.TrimPrefix(parts[0], "ref: ")
				if strings.HasPrefix(target, "refs/heads/") {
					defaultBranch = strings.TrimPrefix(target, "refs/heads/")
				}
			}
			continue
		}
		parts := strings.SplitN(line, "\t", lsRemoteFields)
		if len(parts) == lsRemoteFields && parts[0] != "" && parts[1] != "" {
			refs[parts[1]] = parts[0]
		}
	}

	return refs, defaultBranch
}

// systemGitSmartFetch is the ADO path for SmartFetch. It uses system git to list
// remote refs and fetch the target (or default) branch, then repairs the
// remote-tracking symbolic HEAD so subsequent go-git checkout calls succeed.
func systemGitSmartFetch(
	ctx context.Context,
	repo *gogit.Repository,
	target plumbing.ReferenceName,
	auth transport.AuthMethod,
) (plumbing.ReferenceName, error) {
	logger := log.FromContext(ctx)

	workDir := repoWorkDir(repo)
	if workDir == "" {
		return "", errors.New("systemGitSmartFetch: cannot determine repository path (in-memory storage?)")
	}

	remoteURL, err := remoteOriginURL(repo)
	if err != nil {
		return "", err
	}

	gitEnv, err := newSystemGitEnv(remoteURL, auth)
	if err != nil {
		return "", err
	}
	defer gitEnv.cleanup()

	// List remote refs to discover default branch and target existence.
	out, err := gitEnv.run(ctx, workDir, "ls-remote", "--symref", "origin")
	if err != nil {
		return "", fmt.Errorf("ls-remote for ADO fetch: %w", err)
	}

	refs, defaultBranch := parseADOLSRemote(out)
	if len(refs) == 0 {
		logger.Info("ADO remote is empty, nothing to fetch")
		return "", nil
	}

	targetFullStr := target.String()
	_, targetExists := refs[targetFullStr]
	defaultFull := ""
	if defaultBranch != "" {
		defaultFull = "refs/heads/" + defaultBranch
	}

	// Determine which branch to use after fetch.
	var result plumbing.ReferenceName
	switch {
	case targetExists:
		result = target
	case defaultFull != "":
		result = plumbing.ReferenceName(defaultFull)
	default:
		return "", nil
	}

	// Build refspecs (mirrors buildSmartRefSpecs logic).
	remoteName := "origin"
	refSpecs := buildSmartRefSpecs(remoteName, defaultFull, defaultBranch, target, targetExists)
	if len(refSpecs) == 0 {
		return result, nil
	}

	fetchArgs := []string{"fetch", "--prune", "origin"}
	for _, rs := range refSpecs {
		fetchArgs = append(fetchArgs, string(rs))
	}

	logger.Info("ADO system-git fetch", "target", target.Short(), "refspecs", refSpecs)
	if _, err := gitEnv.run(ctx, workDir, fetchArgs...); err != nil {
		return "", fmt.Errorf("system git fetch for ADO: %w", err)
	}

	repairRemoteSymbolicHead(repo, remoteName, defaultBranch)
	return result, nil
}

// systemGitPushAtomic is the ADO path for PushAtomic. It verifies that rootBranch
// has not moved (concurrent-update guard), then pushes the current branch with a
// force-with-lease so the push is rejected if another actor pushed concurrently.
func systemGitPushAtomic(
	ctx context.Context,
	repo *gogit.Repository,
	rootHash plumbing.Hash,
	rootBranch plumbing.ReferenceName,
	auth transport.AuthMethod,
) error {
	logger := log.FromContext(ctx)

	workDir := repoWorkDir(repo)
	if workDir == "" {
		return errors.New("systemGitPushAtomic: cannot determine repository path (in-memory storage?)")
	}

	remoteURL, err := remoteOriginURL(repo)
	if err != nil {
		return err
	}

	branch, localHash, err := GetCurrentBranch(repo)
	if err != nil {
		return fmt.Errorf("get current branch: %w", err)
	}

	gitEnv, err := newSystemGitEnv(remoteURL, auth)
	if err != nil {
		return err
	}
	defer gitEnv.cleanup()

	// Query current remote state for the root branch and target branch.
	lsOut, err := gitEnv.run(ctx, workDir,
		"ls-remote", "origin",
		rootBranch.String(), branch.String(),
	)
	if err != nil {
		return fmt.Errorf("ls-remote for ADO push: %w", err)
	}

	remoteRefs, _ := parseADOLSRemote(lsOut)
	remoteRootHash := remoteRefs[rootBranch.String()]
	remoteBranchHash := remoteRefs[branch.String()]

	// Concurrency guard: abort if the root branch has been force-pushed since we fetched.
	if !rootHash.IsZero() && remoteRootHash != rootHash.String() {
		return errors.New("remote received unknown updates")
	}

	// Nothing to push.
	if remoteBranchHash == localHash.String() {
		logger.Info("ADO remote already up-to-date", "branch", branch.Short())
		return nil
	}

	branchShort := branch.Short()
	pushArgs := []string{"push"}

	if remoteBranchHash != "" {
		// Branch exists on remote: use force-with-lease to detect concurrent pushes.
		pushArgs = append(pushArgs,
			fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", branchShort, remoteBranchHash),
		)
	}

	pushArgs = append(pushArgs, "origin", fmt.Sprintf("HEAD:refs/heads/%s", branchShort))

	logger.Info("ADO system-git push", "branch", branchShort, "hash", localHash)
	if _, err := gitEnv.run(ctx, workDir, pushArgs...); err != nil {
		return fmt.Errorf("system git push for ADO: %w", err)
	}

	return nil
}

// systemGitCheckRepo is the ADO path for CheckRepo. It uses `git ls-remote --symref`
// to retrieve repository metadata without a go-git transport session.
func systemGitCheckRepo(ctx context.Context, repoURL string, auth transport.AuthMethod) (*RepoInfo, error) {
	logger := log.FromContext(ctx)

	gitEnv, err := newSystemGitEnv(repoURL, auth)
	if err != nil {
		return nil, err
	}
	defer gitEnv.cleanup()

	out, err := gitEnv.run(ctx, "", "ls-remote", "--symref", repoURL)
	if err != nil {
		return nil, fmt.Errorf("ls-remote for ADO check: %w", err)
	}

	if len(bytes.TrimSpace(out)) == 0 {
		logger.Info("ADO repository is empty", "url", repoURL)
		return &RepoInfo{DefaultBranch: nil, RemoteBranchCount: 0}, nil
	}

	refs, defaultBranch := parseADOLSRemote(out)

	info := &RepoInfo{}
	for refName := range refs {
		if strings.HasPrefix(refName, "refs/heads/") {
			info.RemoteBranchCount++
		}
	}

	if defaultBranch != "" {
		defaultFullRef := "refs/heads/" + defaultBranch
		sha := refs[defaultFullRef]
		info.DefaultBranch = &BranchInfo{
			ShortName: defaultBranch,
			Sha:       sha,
			Unborn:    sha == "",
		}
	}

	logger.V(1).Info("ADO repository check completed", "remoteBranches", info.RemoteBranchCount)
	return info, nil
}
