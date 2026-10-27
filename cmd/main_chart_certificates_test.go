// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The default install mints no certificate, so it renders no cert-manager object at all, and installs
// on a cluster without cert-manager. The self-signed Issuer renders exactly when a certificate needs
// it: attribution with audit TLS, or metrics TLS.
func TestChart_TheIssuerRendersOnlyForACertificate(t *testing.T) {
	_, err := os.ReadFile("../charts/gitops-reverser/templates/certificates.yaml")
	require.NoError(t, err)
	kinds := func(setValues ...string) []string {
		var out []string
		for _, template := range []string{"templates/certificates.yaml", "templates/audit-certificates.yaml"} {
			for _, doc := range helmTemplateDocs(t, template, setValues...) {
				for _, line := range strings.Split(doc, "\n") {
					if kind, ok := strings.CutPrefix(line, "kind: "); ok {
						out = append(out, kind)
					}
				}
			}
		}
		return out
	}

	assert.Empty(t, kinds(), "the default install needs no cert-manager")
	assert.Contains(t, kinds("servers.metrics.tls.enabled=true"), "Issuer")
	assert.Contains(t, kinds("attribution.enabled=true", "queue.redis.addr=valkey:6379"), "Issuer")
	assert.NotContains(t, kinds("attribution.enabled=true", "queue.redis.addr=valkey:6379",
		"servers.audit.tls.certManager=false"), "Issuer", "Secrets you bring yourself need no Issuer")
}
