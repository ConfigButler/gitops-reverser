// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// This file is the kcp harness for the source-cluster corner. kcp (installed by Flux via
// test/e2e/setup/kcp) gives us cheap LOGICAL clusters — workspaces — each a real Kubernetes
// API with its OWN namespaces and CRDs. A GitTarget mirrors a workspace exactly like any
// remote cluster: its spec.kubeConfig.secretRef names a kubeconfig that points at the kcp
// front-proxy at /clusters/<workspace-hash>. Because kcp allows isolated copies of the same
// Kind across workspaces, three workspaces serving the same namespace + resource is the
// cheapest possible proof that gitops-reverser keys everything by SOURCE CLUSTER.
//
// Two forms of every workspace kubeconfig:
//   - test-runner form  — server https://127.0.0.1:<port>/clusters/<hash>, reached through a
//     port-forward, with tls-server-name set to the front-proxy's real SAN. Used by the specs
//     to provision workspaces and create resources inside them.
//   - operator form      — server https://frontproxy-front-proxy.kcp.svc.cluster.local:6443/
//     clusters/<hash>, the in-cluster Service DNS the operator Pod dials directly (its serving
//     cert carries that SAN, so TLS verifies with kcp's real CA — no insecure-skip-tls-verify,
//     no file-path credentials: the admin kubeconfig is fully embedded and passes the operator's
//     kubeconfig safety checks unchanged). This is what goes into the GitTarget's Secret.

const (
	kcpNamespace           = "kcp"
	kcpFrontProxyService   = "frontproxy-front-proxy"
	kcpFrontProxySNI       = "frontproxy-front-proxy.kcp.svc.cluster.local"
	kcpFrontProxyInCluster = "https://frontproxy-front-proxy.kcp.svc.cluster.local:6443"
	kcpAdminSecret         = "kcp-admin-kubeconfig"
	// kcpLocalPort is the fixed local port the source-cluster suite forwards the front-proxy to.
	// Fixed because the suite is Ordered and runs single-process (SOURCE_CLUSTER_GINKGO_PROCS=1).
	kcpLocalPort = 16443
)

// kcpAvailable reports whether the kcp control plane is installed (the source-cluster corner's
// prepare ran). The suite skips rather than fails when it is absent, so a default `task test-e2e`
// (which does not install kcp) does not turn red on this corner.
func kcpAvailable() bool {
	_, err := kubectlRun("-n", kcpNamespace, "get", "secret", kcpAdminSecret)
	return err == nil
}

// kcpTunnel is a persistent port-forward to the kcp front-proxy plus the derived kubeconfigs the
// test runner drives kcp with. Created once per source-cluster suite (BeforeAll), stopped in
// AfterAll.
type kcpTunnel struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	// admin is the raw admin kubeconfig from Secret kcp/kcp-admin-kubeconfig (embedded certs).
	admin []byte
	// rootKubeconfig points at 127.0.0.1:<port>/clusters/root — where Workspaces are created.
	rootKubeconfig string
	workdir        string
}

// startKcpTunnel reads the admin kubeconfig, opens the front-proxy port-forward, and derives the
// root-workspace kubeconfig.
func startKcpTunnel() *kcpTunnel {
	GinkgoHelper()
	encoded, err := kubectlRun("-n", kcpNamespace, "get", "secret", kcpAdminSecret,
		"-o", "jsonpath={.data.kubeconfig}")
	Expect(err).NotTo(HaveOccurred(), "read kcp admin kubeconfig Secret")
	admin, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	Expect(err).NotTo(HaveOccurred(), "decode kcp admin kubeconfig")

	workdir, err := os.MkdirTemp("", "kcp-e2e-*")
	Expect(err).NotTo(HaveOccurred())

	cmd, cancel := startKcpPortForward()

	t := &kcpTunnel{cmd: cmd, cancel: cancel, admin: admin, workdir: workdir}
	t.rootKubeconfig = t.writeDerivedKubeconfig("root",
		fmt.Sprintf("https://127.0.0.1:%d/clusters/root", kcpLocalPort), kcpFrontProxySNI)
	return t
}

// startKcpPortForward forwards the front-proxy Service to the fixed local port and waits for it
// to accept connections.
func startKcpPortForward() (*exec.Cmd, context.CancelFunc) {
	GinkgoHelper()
	ctx, cancel := context.WithCancel(context.Background())
	args := kubectlArgs("-n", kcpNamespace, "port-forward",
		"svc/"+kcpFrontProxyService, fmt.Sprintf("%d:6443", kcpLocalPort))
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Start()).To(Succeed(), "start kcp front-proxy port-forward")

	Eventually(func() error {
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", kcpLocalPort), time.Second)
		if dialErr == nil {
			_ = conn.Close()
		}
		return dialErr
	}).WithTimeout(30*time.Second).WithPolling(500*time.Millisecond).
		Should(Succeed(), "kcp front-proxy port-forward never became reachable")

	return cmd, cancel
}

func (t *kcpTunnel) stop() {
	if t == nil {
		return
	}
	if t.cancel != nil {
		t.cancel()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
	}
	_ = os.RemoveAll(t.workdir)
}

// createWorkspace applies a universal-type Workspace under root, waits for it to be Ready, and
// returns its logical-cluster hash — the id both kubeconfig forms address as /clusters/<hash>.
func (t *kcpTunnel) createWorkspace(name string) string {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: tenancy.kcp.io/v1alpha1
kind: Workspace
metadata:
  name: %s
spec:
  type:
    name: universal
    path: root
`, name)
	_, err := kubectlWithKubeconfig(t.rootKubeconfig, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "create kcp workspace %q", name)

	var hash string
	Eventually(func(g Gomega) {
		out, getErr := kubectlWithKubeconfig(t.rootKubeconfig, "", "get", "workspace", name,
			"-o", `jsonpath={.status.phase}{" "}{.spec.cluster}`)
		g.Expect(getErr).NotTo(HaveOccurred())
		fields := strings.Fields(out)
		g.Expect(fields).To(HaveLen(2), "workspace %q not yet assigned a cluster (%q)", name, out)
		g.Expect(fields[0]).To(Equal("Ready"), "workspace %q phase", name)
		hash = fields[1]
	}).WithTimeout(90*time.Second).WithPolling(2*time.Second).
		Should(Succeed(), "kcp workspace %q never became Ready", name)
	return hash
}

// deleteWorkspace best-effort removes a workspace on suite teardown.
func (t *kcpTunnel) deleteWorkspace(name string) {
	_, _ = kubectlWithKubeconfig(t.rootKubeconfig, "", "delete", "workspace", name, "--ignore-not-found")
}

// cleanupWorkspaceTarget tears down everything a workspace-mirror spec created, in dependency
// order: the WatchRule and GitTarget FIRST (so the operator forgets the source cluster and stops
// watching before its workspace disappears), then the Secret, then the workspace. Deleting these
// between specs matters — a GitTarget left pointing at a deleted workspace churns the operator
// (failing discovery every reconcile) and can starve a following spec's initial resync.
func (t *kcpTunnel) cleanupWorkspaceTarget(ns, ws string) {
	_, _ = kubectlRunInNamespace(ns, "delete", "watchrule", ws+"-rule", "--ignore-not-found")
	_, _ = kubectlRunInNamespace(ns, "delete", "gittarget", ws+"-target", "--ignore-not-found")
	_, _ = kubectlRunInNamespace(ns, "delete", "secret", ws+"-kubeconfig", "--ignore-not-found")
	t.deleteWorkspace(ws)
}

// wsKubectl runs kubectl against a workspace (test-runner form), lazily writing that workspace's
// runner kubeconfig on first use.
func (t *kcpTunnel) wsKubectl(hash string, args ...string) (string, error) {
	kc := filepath.Join(t.workdir, "ws-"+hash+".kubeconfig")
	if _, err := os.Stat(kc); err != nil {
		t.writeDerivedKubeconfigAt(kc,
			fmt.Sprintf("https://127.0.0.1:%d/clusters/%s", kcpLocalPort, hash), kcpFrontProxySNI)
	}
	return kubectlWithKubeconfig(kc, "", args...)
}

// operatorKubeConfig returns the kubeconfig the OPERATOR uses to watch a workspace: the in-cluster
// front-proxy Service DNS at /clusters/<hash>, with the credential embedded. It is what a
// GitTarget's spec.kubeConfig.secretRef Secret carries.
func (t *kcpTunnel) operatorKubeConfig(hash string) string {
	GinkgoHelper()
	raw, err := deriveKubeConfig(t.admin, kcpFrontProxyInCluster+"/clusters/"+hash, "")
	Expect(err).NotTo(HaveOccurred(), "derive operator kubeconfig for workspace %q", hash)
	return string(raw)
}

func (t *kcpTunnel) writeDerivedKubeconfig(name, server, sni string) string {
	GinkgoHelper()
	path := filepath.Join(t.workdir, name+".kubeconfig")
	t.writeDerivedKubeconfigAt(path, server, sni)
	return path
}

func (t *kcpTunnel) writeDerivedKubeconfigAt(path, server, sni string) {
	GinkgoHelper()
	raw, err := deriveKubeConfig(t.admin, server, sni)
	Expect(err).NotTo(HaveOccurred(), "derive kubeconfig for %s", server)
	Expect(os.WriteFile(path, raw, 0o600)).To(Succeed())
}

// deriveKubeConfig rewrites every cluster entry's server (and, when sni is non-empty, its
// tls-server-name) in a kcp kubeconfig, preserving the embedded credential. It is the Go
// equivalent of the reference repo's kcp-kubectl.sh rewrite — done in-process so the harness
// needs no python.
func deriveKubeConfig(adminRaw []byte, server, sni string) ([]byte, error) {
	cfg, err := clientcmd.Load(adminRaw)
	if err != nil {
		return nil, fmt.Errorf("load kcp admin kubeconfig: %w", err)
	}
	for _, cluster := range cfg.Clusters {
		cluster.Server = server
		if sni != "" {
			cluster.TLSServerName = sni
		}
	}
	out, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("write derived kubeconfig: %w", err)
	}
	return out, nil
}

// kubectlWithKubeconfig runs kubectl against an explicit kubeconfig file (a kcp workspace),
// bypassing the k3d --context the other e2e helpers inject.
func kubectlWithKubeconfig(kubeconfigPath, stdin string, args ...string) (string, error) {
	full := append([]string{"--kubeconfig=" + kubeconfigPath}, args...)
	cmd := exec.CommandContext(context.Background(), "kubectl", full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return utils.Run(cmd)
}
