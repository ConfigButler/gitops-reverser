# Azure DevOps and `multi_ack`: what was actually measured

> **facts** — durable reference. Index: [`../INDEX.md`](../INDEX.md)
>
> Measured 2026-07-30 against `dev.azure.com` with a Personal Access Token. The folklore is nearly
> right, in a way that makes it easy to reproduce the wrong thing; the
> [wrong turn](#a-wrong-turn-worth-recording) is recorded so nobody repeats it.

## The rule

**Azure DevOps answers HTTP 400 to an `upload-pack` request whose capability list omits both
`multi_ack` and `multi_ack_detailed`:**

```text
TF401041: The Git protocol sent is not as expected (Clients must support multi-ack...)
```

The trigger is a capability list that omits it, not the absence of negotiation. Measured, one request
shape per row, all against the same repository and tip:

| `want` line | HTTP |
|---|---|
| `want <sha>` — no capability list at all | **200** |
| `want <sha> side-band-64k ofs-delta agent=git/2.39.5` | **400 `TF401041`** |
| `want <sha> side-band-64k` | **400 `TF401041`** |
| `want <sha> agent=git/2.39.5` | **400 `TF401041`** |
| `want <sha> multi_ack side-band-64k ofs-delta agent=git/2.39.5` | **200** |
| `want <sha> multi_ack_detailed side-band-64k ofs-delta` | **200** |

Either capability satisfies it. The first row is the trap: a bare `want` with no capability list is
accepted, and no real client sends that, so probing with one measures nothing useful.

Reproduce a row with:

```bash
SHA=$(git ls-remote <repo-url> HEAD | awk '{print $1}')
CAPS=" side-band-64k ofs-delta"          # add multi_ack here to flip the result
python3 -c "
import sys
want='want $SHA'+sys.argv[1]+'\n'
sys.stdout.write('%04x%s' % (len(want)+4, want) + '0000' + '%04xdone\n' % 9)" "$CAPS" |
curl -s -o /dev/stdout -w '\nHTTP %{http_code}\n' \
  -H "Authorization: Basic $(printf ':%s' "$PAT" | base64 -w0)" \
  -H 'Content-Type: application/x-git-upload-pack-request' --data-binary @- \
  "<repo-url>/git-upload-pack"
```

## What it does to go-git v5

go-git v5 keeps `MultiACK` and `MultiACKDetailed` in `transport.UnsupportedCapabilities` and deletes
them from the server's advertisement while parsing it, so
`packp.NewUploadPackRequestFromCapabilities` never sees them and never asks for them. Every fetch it
sends therefore carries a capability list without `multi_ack`, which is precisely the rejected shape.

Run directly against Azure DevOps with `go-git/v5@v5.19.1`, the version this project pinned before
[#297](https://github.com/ConfigButler/gitops-reverser/pull/297):

```text
== step 1: clone (want/done, no have lines) ==
CLONE FAILED: unexpected client error: unexpected requesting
"https://dev.azure.com/<org>/<project>/_git/<repo>/git-upload-pack" status code: 400
```

**Not even the clone works.** Upstream's note that the initial clone succeeds holds only *after*
trimming `UnsupportedCapabilities`, which makes the client advertise the capability again. That trim
is Flux's four-line workaround; it fixes the request, but v5 still cannot decode the multi-ACK
*responses* the server may then send (`srvresp.go` carries a `TODO` for exactly that), which is why
Flux only ever clones. v6 implements the capability properly.

## Why canonical git never notices

`git upload-pack` advertises `multi_ack` and `multi_ack_detailed`, so canonical git always negotiates
one of them and never constructs the rejected shape. Verified against a local repository with
protocol v0 forced:

```text
<oid> refs/heads/main\0multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since
  deepen-not deepen-relative no-progress include-tag multi_ack_detailed object-format=sha1
  agent=git/2.39.5
```

This is also what makes a local `git-http-backend` a usable stand-in for ADO in tests: it is a real
multi_ack-speaking server, so putting a proxy in front that enforces ADO's rule reproduces both halves
of the problem without needing a tenant.

`receive-pack` is a different protocol and has no `multi_ack` at all, measured:

```text
<oid> refs/heads/main\0report-status report-status-v2 delete-refs side-band-64k quiet atomic
  ofs-delta object-format=sha1 agent=git/2.39.5
```

So **pushing to Azure DevOps was never affected**, on any version.

## A wrong turn worth recording

A bare `want <sha>` with no capability list returns 200, which briefly looked like evidence that the
six-year-old bug reports were wrong. It is the one row above ADO accepts, and no client sends it.
Running go-git v5 against the same repository produced the reported 400 immediately.

Probe with the shape the real client sends: a hand-rolled request tests the request you built, not
the behaviour you are attributing to it.

## Sources

- [go-git#64](https://github.com/go-git/go-git/issues/64) — the original Azure DevOps report, open
  since 2019.
- [fluxcd/source-controller#104](https://github.com/fluxcd/source-controller/issues/104) — Flux
  hitting the same wall, and the comment thread that leads to their workaround.
- [go-git#1204](https://github.com/go-git/go-git/pull/1204) — the `multi_ack` implementation, merged
  for v6. Upstream subsequently deleted their `_examples/azure_devops` with the message *"Since the
  multi_ack implementation (#1204), Azure DevOps works out of the box, no longer requiring code
  changes."*
- go-git v5 `_examples/azure_devops/main.go` — the `UnsupportedCapabilities` trim, and the warning
  that additional fetches will yield issues.
- go-git v5 `plumbing/protocol/packp/srvresp.go` — the unimplemented multi-ACK response decoding.
- `fluxcd/pkg` `pkg/git/gogit/client.go` — the trim applied in an `init()`, alongside a client that
  only ever clones.
- [Git protocol capabilities](https://git-scm.com/docs/protocol-capabilities) — `multi_ack` and
  `multi_ack_detailed` are `upload-pack` capabilities; `receive-pack` has neither.

## Where this is exercised

| Test | Needs a tenant? |
|---|---|
| [`internal/git/ado_multiack_test.go`](../../internal/git/ado_multiack_test.go) | no — a local `git-http-backend` behind a proxy enforcing the rule, runs in CI |
| [`internal/git/ado_live_test.go`](../../internal/git/ado_live_test.go) | yes — including `TestADOLive_StillRequiresMultiAck`, the canary that fails if Microsoft ever fixes this |
| [`test/e2e/ado_e2e_test.go`](../../test/e2e/ado_e2e_test.go) | yes — the operator mirroring into a real ADO repository |

The simulator is deliberately stricter than ADO: it rejects any `upload-pack` POST carrying neither
`multi_ack` nor `multi_ack_detailed` — its check matches the `multi_ack` prefix, so either satisfies
it, exactly as ADO does — including the bare-`want` shape ADO accepts. That difference does not matter for what it
gates, and the strictness is what keeps it simple.
