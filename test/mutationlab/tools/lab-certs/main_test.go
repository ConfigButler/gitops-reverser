// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

func testOptions(t *testing.T) options {
	t.Helper()
	dir := t.TempDir()
	return options{
		certDir:    filepath.Join(dir, "certs"),
		kubeconfig: filepath.Join(dir, "audit", "webhook-config.yaml"),
		service:    "mutation-capture-lab",
		namespace:  "mutation-capture-lab",
		auditURL:   "https://127.0.0.1:30444/audit-webhook",
	}
}

// TestEnsure_HandshakeVerifiesBothSides is the property the lab depends on: a server presenting
// the issued serving certificate, requiring a client certificate from the issued CA, accepts the
// client the audit kubeconfig describes, verifying the server under the kubeconfig's
// tls-server-name.
func TestEnsure_HandshakeVerifiesBothSides(t *testing.T) {
	o := testOptions(t)
	issued, err := ensure(o, time.Now())
	if err != nil || !issued {
		t.Fatalf("ensure = %v, %v; want issued", issued, err)
	}

	cert, err := tls.LoadX509KeyPair(filepath.Join(o.certDir, servingCert), filepath.Join(o.certDir, servingKey))
	if err != nil {
		t.Fatalf("load serving pair: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(o.certDir, caFile))
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA file holds no certificate")
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	srv.StartTLS()
	defer srv.Close()

	cfg, err := clientcmd.LoadFromFile(o.kubeconfig)
	if err != nil {
		t.Fatalf("load audit kubeconfig: %v", err)
	}
	ctx := cfg.Contexts[cfg.CurrentContext]
	cluster, user := cfg.Clusters[ctx.Cluster], cfg.AuthInfos[ctx.AuthInfo]
	if cluster.Server != o.auditURL {
		t.Errorf("server = %q, want %q", cluster.Server, o.auditURL)
	}
	if cluster.TLSServerName != "mutation-capture-lab.mutation-capture-lab.svc" {
		t.Errorf("tls-server-name = %q", cluster.TLSServerName)
	}
	clientPair, err := tls.X509KeyPair(user.ClientCertificateData, user.ClientKeyData)
	if err != nil {
		t.Fatalf("client pair from kubeconfig: %v", err)
	}
	trust := x509.NewCertPool()
	trust.AppendCertsFromPEM(cluster.CertificateAuthorityData)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      trust,
		ServerName:   cluster.TLSServerName,
		Certificates: []tls.Certificate{clientPair},
	}}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("mutual TLS request: %v", err)
	}
	_ = resp.Body.Close()

	// Without the client certificate the server refuses the handshake, which is what makes the
	// lab's audit receiver as strict as a production one.
	anonymous := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: trust, ServerName: cluster.TLSServerName,
	}}}
	if resp, err := anonymous.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("request without a client certificate succeeded")
	}
}

func TestEnsure_KeepsACompleteSet(t *testing.T) {
	o := testOptions(t)
	if _, err := ensure(o, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(o.certDir, caFile))
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ensure(o, time.Now())
	if err != nil || issued {
		t.Fatalf("second ensure = %v, %v; want kept", issued, err)
	}
	after, err := os.ReadFile(filepath.Join(o.certDir, caFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a complete set was reissued")
	}
}

func TestEnsure_ReissuesAPartialSet(t *testing.T) {
	o := testOptions(t)
	if _, err := ensure(o, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(o.certDir, caFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(o.kubeconfig); err != nil {
		t.Fatal(err)
	}
	issued, err := ensure(o, time.Now())
	if err != nil || !issued {
		t.Fatalf("ensure after a removal = %v, %v; want issued", issued, err)
	}
	after, err := os.ReadFile(filepath.Join(o.certDir, caFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("a partial set kept its old CA; the new kubeconfig would not match the serving certificate")
	}
}

func TestEnsure_KeyFilesArePrivate(t *testing.T) {
	o := testOptions(t)
	if _, err := ensure(o, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(o.certDir, servingKey), o.kubeconfig} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != keyFileMode {
			t.Errorf("%s mode = %o, want %o", p, mode, keyFileMode)
		}
	}
}
