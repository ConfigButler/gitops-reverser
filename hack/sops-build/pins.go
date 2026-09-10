//go:build pins

// SPDX-License-Identifier: Apache-2.0

// Package pins exists only to hold the module in this directory to a set of
// dependency versions NEWER than the ones SOPS itself requires.
//
// Nothing here is ever compiled: the `pins` build tag is never set. The file is
// here because `go mod tidy` reads imports under every build tag, so these
// imports keep the packages below as DIRECT requirements in go.mod. That is the
// whole point — Dependabot's gomod ecosystem raises version updates for direct
// requirements, so a future CVE in one of these lands as a pull request instead
// of waiting for SOPS to cut a release. As plain indirect requirements they
// would only move on a security advisory, and only if Dependabot saw it.
//
// Each import is a package whose module shipped a fix that the pinned SOPS
// release was built too early to contain. Removing an entry is safe once SOPS
// requires the fixed version itself; the image scan is what would catch a
// mistake either way.
package pins

import (
	// The tool directive above names cmd/sops, which is a main package and so
	// cannot be imported. Importing the library root instead is what makes
	// github.com/getsops/sops/v3 a DIRECT requirement, which is what gets SOPS
	// itself a Dependabot pull request when upstream tags a release. Without it
	// the module sits at whatever version the tool directive resolved to and
	// nobody is told a newer one exists.
	_ "github.com/getsops/sops/v3"

	// golang.org/x/crypto: CVE-2026-56854 (CRITICAL, ssh authorized_keys `from=`
	// bypass), plus CVE-2026-56855 and CVE-2026-78662. Fixed in v0.56.0;
	// SOPS v3.13.3 requires v0.54.0.
	_ "golang.org/x/crypto/ssh"

	// golang.org/x/net: carried the idna and dnsmessage fixes ahead of the
	// toolchain bump. Fixed in v0.58.0; SOPS v3.13.3 requires v0.57.0.
	_ "golang.org/x/net/http2"

	// google.golang.org/grpc: CVE-2026-84303, CVE-2026-84304 and CVE-2026-84445.
	// Fixed in v1.83.2; SOPS v3.13.3 requires v1.82.1.
	_ "google.golang.org/grpc"
)
