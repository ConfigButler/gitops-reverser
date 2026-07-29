// SPDX-License-Identifier: Apache-2.0

package git

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/http"
	gossh "golang.org/x/crypto/ssh"

	sshpkg "github.com/ConfigButler/gitops-reverser/internal/ssh"
)

func TestIsADOURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://dev.azure.com/org/project/_git/repo", true},
		{"https://DEV.AZURE.COM/org/project/_git/repo", true},
		{"https://org.visualstudio.com/project/_git/repo", true},
		{"ssh://git@ssh.dev.azure.com:v3/org/project/repo", true},
		{"git@ssh.dev.azure.com:v3/org/project/repo", true},
		{"https://github.com/org/repo.git", false},
		{"https://gitlab.com/org/repo.git", false},
		{"git@github.com:org/repo.git", false},
		{"https://bitbucket.org/org/repo.git", false},
		{"", false},
		// Injection attempts must not match via substring.
		{"ext::sh -c 'touch /tmp/x #dev.azure.com'", false},
		{"ext::dev.azure.com", false},
		{"file:///dev.azure.com", false},
		{"https://attacker.com/dev.azure.com", false},
	}

	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			if got := IsADOURL(tc.url); got != tc.want {
				t.Errorf("IsADOURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

func TestNewSystemGitEnv_BasicAuth(t *testing.T) {
	auth := &http.BasicAuth{Username: "user", Password: "test-password"}
	env, err := newSystemGitEnv("https://dev.azure.com/org/proj/_git/repo", auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer env.cleanup()

	var cfgPath string
	for _, v := range env.vars {
		if strings.HasPrefix(v, "GIT_CONFIG_GLOBAL=") {
			cfgPath = strings.TrimPrefix(v, "GIT_CONFIG_GLOBAL=")
		}
	}
	if cfgPath == "" {
		t.Fatal("GIT_CONFIG_GLOBAL not set")
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "extraHeader") {
		t.Error("gitconfig must contain http.extraHeader")
	}
	if !strings.Contains(content, "Basic ") {
		t.Error("gitconfig must use Basic scheme")
	}
	// Credential must appear base64-encoded, not as plain text in the header.
	if strings.Contains(content, "test-password") {
		t.Error("raw password must not appear in gitconfig; should be base64-encoded")
	}

	// File must be user-only readable.
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat gitconfig: %v", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Errorf("gitconfig is group/world readable: mode %o", info.Mode().Perm())
	}
}

func TestNewSystemGitEnv_TokenAuth(t *testing.T) {
	auth := &http.TokenAuth{Token: "test-token"}
	env, err := newSystemGitEnv("https://dev.azure.com/org/proj/_git/repo", auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer env.cleanup()

	var cfgPath string
	for _, v := range env.vars {
		if strings.HasPrefix(v, "GIT_CONFIG_GLOBAL=") {
			cfgPath = strings.TrimPrefix(v, "GIT_CONFIG_GLOBAL=")
		}
	}

	if cfgPath == "" {
		t.Fatal("GIT_CONFIG_GLOBAL not set")
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	if !strings.Contains(string(data), "test-token") {
		t.Error("gitconfig must contain the bearer token")
	}
	if !strings.Contains(string(data), "extraHeader") {
		t.Error("gitconfig must contain http.extraHeader")
	}

	// File must be user-only readable.
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat gitconfig: %v", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Errorf("gitconfig is group/world readable: mode %o", info.Mode().Perm())
	}
}

func TestNewSystemGitEnv_Anonymous(t *testing.T) {
	env, err := newSystemGitEnv("https://dev.azure.com/org/proj/_git/repo", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	env.cleanup()

	for _, v := range env.vars {
		if strings.HasPrefix(v, "GIT_ASKPASS=") || strings.HasPrefix(v, "GIT_CONFIG_GLOBAL=") {
			t.Errorf("unexpected env var for anonymous auth: %s", v)
		}
	}
}

type unsupportedAuth struct{}

func (unsupportedAuth) Name() string   { return "unsupported" }
func (unsupportedAuth) String() string { return "unsupported" }

func TestNewSystemGitEnv_UnsupportedAuth(t *testing.T) {
	_, err := newSystemGitEnv("https://dev.azure.com/org/proj/_git/repo", unsupportedAuth{})
	if err == nil {
		t.Fatal("expected error for unsupported auth type, got nil")
	}
}

func TestNewSystemGitEnv_Cleanup(t *testing.T) {
	auth := &http.BasicAuth{Username: "u", Password: "p"}
	env, err := newSystemGitEnv("https://dev.azure.com/org/proj/_git/repo", auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the temp dir from the gitconfig path.
	var tmpDir string
	for _, v := range env.vars {
		if strings.HasPrefix(v, "GIT_CONFIG_GLOBAL=") {
			tmpDir = filepath.Dir(strings.TrimPrefix(v, "GIT_CONFIG_GLOBAL="))
		}
	}
	if tmpDir == "" {
		t.Fatal("could not find temp dir")
	}

	env.cleanup()

	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Errorf("temp dir %q still exists after cleanup", tmpDir)
	}
}

func TestParseADOLSRemote(t *testing.T) {
	raw := []byte("ref: refs/heads/main\tHEAD\n" +
		"abc123def456\tHEAD\n" +
		"abc123def456\trefs/heads/main\n" +
		"deadbeef0000\trefs/heads/feature\n")

	refs, defaultBranch := parseADOLSRemote(raw)

	if defaultBranch != "main" {
		t.Errorf("defaultBranch = %q, want %q", defaultBranch, "main")
	}
	if refs["refs/heads/main"] != "abc123def456" {
		t.Errorf("refs[main] = %q, want %q", refs["refs/heads/main"], "abc123def456")
	}
	if refs["refs/heads/feature"] != "deadbeef0000" {
		t.Errorf("refs[feature] = %q, want %q", refs["refs/heads/feature"], "deadbeef0000")
	}
}

func TestParseADOLSRemote_Empty(t *testing.T) {
	refs, defaultBranch := parseADOLSRemote(nil)
	if defaultBranch != "" {
		t.Errorf("defaultBranch = %q, want empty", defaultBranch)
	}
	if len(refs) != 0 {
		t.Errorf("refs has %d entries, want 0", len(refs))
	}
}

// generateTestSSHKey returns a PEM-encoded unencrypted ECDSA private key and a
// known_hosts line for a fake host, suitable for unit-testing newSystemGitEnv.
func generateTestSSHKey(t *testing.T) ([]byte, string) {
	t.Helper()
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	block, err := gossh.MarshalPrivateKey(privKey, "")
	if err != nil {
		t.Fatalf("marshal test key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(block)

	// Build a valid known_hosts line using a separate host key.
	hostKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	pubKey, err := gossh.NewPublicKey(hostKey.Public())
	if err != nil {
		t.Fatalf("derive public key: %v", err)
	}
	knownHostsLine := "ssh.dev.azure.com " + strings.TrimRight(string(gossh.MarshalAuthorizedKey(pubKey)), "\n")
	return keyPEM, knownHostsLine
}

// assertSSHFilesNotWorldReadable checks that every path in sshCmd that lives under
// the reverser-git temp dir is not group- or world-readable.
// singleQuotedPath matches every single-quoted token in a shell command string,
// e.g. '-i '/tmp/reverser-git-x/id_key'' and 'UserKnownHostsFile='/tmp/.../''.
var singleQuotedPath = regexp.MustCompile(`'([^']+)'`)

func assertSSHFilesNotWorldReadable(t *testing.T, sshCmd string) {
	t.Helper()
	for _, m := range singleQuotedPath.FindAllStringSubmatch(sshCmd, -1) {
		path := m[1]
		if !strings.Contains(path, "reverser-git") {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %q: %v", path, err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("file %q is group/world readable: %o", path, info.Mode().Perm())
		}
	}
}

func TestNewSystemGitEnv_SSHAuth_WithKnownHosts(t *testing.T) {
	keyPEM, knownHostsLine := generateTestSSHKey(t)

	auth, err := sshpkg.NewSSHKeyAuth(string(keyPEM), "", knownHostsLine, false)
	if err != nil {
		t.Fatalf("NewSSHKeyAuth: %v", err)
	}

	env, err := newSystemGitEnv("ssh://git@ssh.dev.azure.com:v3/org/proj/repo", auth)
	if err != nil {
		t.Fatalf("newSystemGitEnv: %v", err)
	}
	defer env.cleanup()

	var sshCmd string
	for _, v := range env.vars {
		if strings.HasPrefix(v, "GIT_SSH_COMMAND=") {
			sshCmd = strings.TrimPrefix(v, "GIT_SSH_COMMAND=")
		}
	}
	if sshCmd == "" {
		t.Fatal("GIT_SSH_COMMAND not set")
	}
	if !strings.Contains(sshCmd, "-i ") {
		t.Error("GIT_SSH_COMMAND must include -i <keyfile>")
	}
	if !strings.Contains(sshCmd, "StrictHostKeyChecking=yes") {
		t.Error("GIT_SSH_COMMAND must use StrictHostKeyChecking=yes when known_hosts is provided")
	}
	if !strings.Contains(sshCmd, "UserKnownHostsFile=") {
		t.Error("GIT_SSH_COMMAND must reference a UserKnownHostsFile")
	}
	assertSSHFilesNotWorldReadable(t, sshCmd)
}

func TestNewSystemGitEnv_SSHAuth_WithoutKnownHosts(t *testing.T) {
	keyPEM, _ := generateTestSSHKey(t)
	auth, err := sshpkg.NewSSHKeyAuth(string(keyPEM), "", "", true) // allowMissing=true
	if err != nil {
		t.Fatalf("NewSSHKeyAuth: %v", err)
	}

	env, err := newSystemGitEnv("ssh://git@ssh.dev.azure.com:v3/org/proj/repo", auth)
	if err != nil {
		t.Fatalf("newSystemGitEnv: %v", err)
	}
	defer env.cleanup()

	var sshCmd string
	for _, v := range env.vars {
		if strings.HasPrefix(v, "GIT_SSH_COMMAND=") {
			sshCmd = strings.TrimPrefix(v, "GIT_SSH_COMMAND=")
		}
	}
	if !strings.Contains(sshCmd, "StrictHostKeyChecking=accept-new") {
		t.Errorf("expected accept-new when no known_hosts, got: %s", sshCmd)
	}
}
