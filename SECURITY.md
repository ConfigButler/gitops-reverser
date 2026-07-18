# Security Policy

GitOps Reverser is early-stage infrastructure software. It handles audit-webhook traffic, Git
credentials, and optionally encrypted Secret material. Security findings are very welcome, but this
project should not be introduced into production without a proper review of the code, deployment
model, and operational risks for your environment.

For what the controller can access and which pieces are sensitive, see
[`docs/security-model.md`](docs/security-model.md).

## Shared audit-ingress trust model

Audit ingress uses mutual TLS, but the current multi-cluster routes use the client certificate to
authenticate membership in the shared audit CA—not to bind a sender to one `ClusterProvider`. A
source that holds the shared client credential can therefore submit audit facts for any configured
provider, whether the provider is selected by `/audit-webhook/<provider>` or by shared-stream
annotation routing.

This is an explicit, accepted operating assumption for now: use shared ingress only when a highly
privileged control plane manages all participating source clusters, protects the shared credential,
and treats those sources as mutually trusted. It is not a tenant-isolation boundary. Deploy separate
instances or keep attribution disabled when one source must not be able to influence another source's
Git author history. Provider-bound client identities may be added if that stronger boundary is needed.

## Reporting a vulnerability

Please do not open a public GitHub issue for security-sensitive reports.

Instead, contact the maintainer directly:

- LinkedIn: [Simon Koudijs](https://www.linkedin.com/in/simonkoudijs/)

When possible, include:

- the affected version or commit
- a short description of the impact
- reproduction steps or a proof of concept
- any relevant logs, manifests, or configuration details

You will receive an acknowledgment as soon as practical. Please allow time for investigation and a
fix before public disclosure.

## Early-stage software notice

This project is still evolving quickly.

- Security findings and responsible reports are appreciated.
- Hardening work is ongoing.
- Deployments should be reviewed carefully before production use.

In other words: very open to findings, but not something to throw into production casually.

## Scope

Security reports are especially relevant for areas such as:

- audit webhook ingestion and authentication
- queue and Valkey/Redis handling
- Git credential handling
- SOPS and Secret processing
- admission webhook validation

## Supported versions

This project is still early-stage software. Security fixes are expected to land on `main` first.
There is no long-term support policy for older releases yet.
