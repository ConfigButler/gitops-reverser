// SPDX-License-Identifier: Apache-2.0

// Command lab-certs issues the mutation-capture lab's certificates before its cluster exists:
// one CA, a serving certificate for the lab's Service, and a client certificate for the API
// server, which it writes into the audit webhook kubeconfig the cluster is created with. The API
// server reads that kubeconfig only at startup, so issuing first is what lets the lab cluster
// come up complete, without the node restart the e2e cluster needs.
//
// It is idempotent: when every output exists it leaves them alone, because the running cluster
// already trusts the CA they were issued from.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	certValidity = 365 * 24 * time.Hour
	keyFileMode  = 0o600
	certFileMode = 0o644
	dirMode      = 0o750
	serialBits   = 128

	caFile         = "ca.crt"
	servingCert    = "tls.crt"
	servingKey     = "tls.key"
	clientIdentity = "kube-apiserver"
)

type options struct {
	certDir    string
	kubeconfig string
	service    string
	namespace  string
	auditURL   string
}

func main() {
	var o options
	flag.StringVar(&o.certDir, "cert-dir", "", "directory to write ca.crt, tls.crt and tls.key into")
	flag.StringVar(&o.kubeconfig, "audit-kubeconfig", "", "path to write the API server's audit webhook kubeconfig")
	flag.StringVar(&o.service, "service", "mutation-capture-lab", "the lab Service the serving certificate names")
	flag.StringVar(&o.namespace, "namespace", "mutation-capture-lab", "the lab Service's namespace")
	flag.StringVar(&o.auditURL, "audit-url", "https://127.0.0.1:30444/audit-webhook",
		"the URL the API server posts audit batches to")
	flag.Parse()

	if o.certDir == "" || o.kubeconfig == "" {
		exitf("--cert-dir and --audit-kubeconfig are required")
	}
	issued, err := ensure(o, time.Now())
	if err != nil {
		exitf("%v", err)
	}
	if issued {
		_, _ = fmt.Fprintf(os.Stdout, "issued lab certificates into %s and %s\n", o.certDir, o.kubeconfig)
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "lab certificates already exist in %s\n", o.certDir)
	}
}

// ensure issues every output unless all of them exist, and reports whether it issued. A partial
// set is reissued whole: a serving certificate from one CA and a kubeconfig from another would
// fail the handshake in a way that looks like a network problem.
func ensure(o options, now time.Time) (bool, error) {
	outputs := []string{
		filepath.Join(o.certDir, caFile),
		filepath.Join(o.certDir, servingCert),
		filepath.Join(o.certDir, servingKey),
		o.kubeconfig,
	}
	if allExist(outputs) {
		return false, nil
	}
	b, err := issue(o, now)
	if err != nil {
		return false, err
	}
	files := map[string]struct {
		data []byte
		mode os.FileMode
	}{
		outputs[0]: {b.caPEM, certFileMode},
		outputs[1]: {b.servingCertPEM, certFileMode},
		outputs[2]: {b.servingKeyPEM, keyFileMode},
		outputs[3]: {b.kubeconfig, keyFileMode},
	}
	for path, f := range files {
		if err := writeFile(path, f.data, f.mode); err != nil {
			return false, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return true, nil
}

func allExist(paths []string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

// bundle is everything issue produces, PEM-encoded.
type bundle struct {
	caPEM          []byte
	servingCertPEM []byte
	servingKeyPEM  []byte
	kubeconfig     []byte
}

// serverName is the DNS name the serving certificate is issued for, and the name the API server
// verifies on every connection: admission and conversion address the Service, and audit, which
// posts to a NodePort on 127.0.0.1, names it as its tls-server-name.
func (o options) serverName() string { return o.service + "." + o.namespace + ".svc" }

func issue(o options, now time.Time) (*bundle, error) {
	caKey, caCert, caDER, err := newCA(now)
	if err != nil {
		return nil, err
	}
	serverName := o.serverName()
	servingDER, servingKey, err := newLeaf(caCert, caKey, now, &x509.Certificate{
		Subject:     pkix.Name{CommonName: serverName},
		DNSNames:    []string{o.service, o.service + "." + o.namespace, serverName, serverName + ".cluster.local"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("serving certificate: %w", err)
	}
	clientDER, clientKey, err := newLeaf(caCert, caKey, now, &x509.Certificate{
		Subject:     pkix.Name{CommonName: clientIdentity},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	}

	caPEM := pemBlock("CERTIFICATE", caDER)
	kubeconfig, err := auditKubeconfig(o.auditURL, serverName, caPEM,
		pemBlock("CERTIFICATE", clientDER), clientKey)
	if err != nil {
		return nil, err
	}
	return &bundle{
		caPEM:          caPEM,
		servingCertPEM: pemBlock("CERTIFICATE", servingDER),
		servingKeyPEM:  servingKey,
		kubeconfig:     kubeconfig,
	}, nil
}

func newCA(now time.Time) (*ecdsa.PrivateKey, *x509.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mutation-capture-lab CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	return key, cert, der, nil
}

// newLeaf signs tmpl with the CA and returns the certificate's DER and its key as PEM.
func newLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, now time.Time, tmpl *x509.Certificate) (
	[]byte, []byte, error,
) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl.SerialNumber = serial
	tmpl.NotBefore = now.Add(-time.Hour)
	tmpl.NotAfter = now.Add(certValidity)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return der, pemBlock("EC PRIVATE KEY", keyDER), nil
}

func newSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, fmt.Errorf("serial number: %w", err)
	}
	return serial, nil
}

// auditKubeconfig is the kubeconfig the API server's audit webhook backend reads: where to post,
// which CA and server name to verify, and the client certificate to present.
func auditKubeconfig(server, serverName string, caPEM, clientCertPEM, clientKeyPEM []byte) ([]byte, error) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["mutation-capture-lab"] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: caPEM,
		TLSServerName:            serverName,
	}
	cfg.AuthInfos[clientIdentity] = &clientcmdapi.AuthInfo{
		ClientCertificateData: clientCertPEM,
		ClientKeyData:         clientKeyPEM,
	}
	cfg.Contexts["audit"] = &clientcmdapi.Context{Cluster: "mutation-capture-lab", AuthInfo: clientIdentity}
	cfg.CurrentContext = "audit"
	out, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("audit kubeconfig: %w", err)
	}
	return out, nil
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func writeFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	return os.WriteFile(path, content, mode)
}

func exitf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
