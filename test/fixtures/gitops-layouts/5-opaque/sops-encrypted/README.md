# sops-encrypted

## What this is

The standard way teams keep Secrets in a public or shared GitOps repo: encrypt
them at rest with [SOPS](https://github.com/getsops/sops), commit the encrypted
files, and let the GitOps controller decrypt in-cluster at apply time. Flux has
first-class SOPS support (`spec.decryption.provider: sops`); Argo CD teams reach
the same outcome with `ksops`, `argocd-vault-plugin`, or a decrypt sidecar. The
private key (here an `age` identity) lives only in the cluster, never in Git.

The defining property of SOPS is **partial** encryption: it encrypts *values*
but leaves keys and document structure in cleartext. So a SOPS-encrypted Secret
is still a syntactically valid Kubernetes object — you can read its `kind`,
`metadata.name`, and `metadata.namespace` — but its `data` values are opaque.

## Layout

```yaml
13-sops-encrypted/
├── README.md
├── .sops.yaml                         # sops creation-rules (NOT a K8s object)
├── apps/
│   └── frontend/
│       ├── deployment.yaml            # KRM: plain, unencrypted Deployment
│       ├── secret.enc.yaml            # KRM: SOPS-encrypted Secret (data opaque)
│       └── kustomization.yaml         # kustomize config (NOT a K8s object)
├── infrastructure/
│   └── secrets/
│       ├── db-credentials.enc.yaml    # KRM: SOPS-encrypted Secret
│       └── kustomization.yaml         # kustomize config (NOT a K8s object)
└── clusters/
    └── production/
        └── apps.yaml                  # Flux Kustomization (+GitRepository),
                                       #   spec.decryption.provider: sops
```yaml

## What makes it structurally distinct

- **A single file is both readable and unreadable.** In `secret.enc.yaml` the
  `apiVersion`, `kind`, `metadata`, and `type` are cleartext and fully
  structural; the values under `data` are `ENC[AES256_GCM,...]` ciphertext.
  Structure is legible; content is not.
- **`.sops.yaml` is YAML that is NOT a Kubernetes object.** It has no
  `apiVersion`/`kind`; it is configuration for the `sops` CLI describing which
  paths to encrypt (`path_regex: .*\.enc\.yaml$`) and which fields
  (`encrypted_regex: ^(data|stringData)$`), and to which `age` recipient.
- **The `sops:` block is metadata about the encryption, not part of the K8s
  object.** It carries `age.recipient`, an armored key-wrap `enc` blob,
  `lastmodified`, a `mac`, and `version`. `kubectl apply` would reject or ignore
  it; it exists only so `sops` can decrypt and verify integrity.
- **The `mac` binds the whole cleartext.** It is a message authentication code
  over the decrypted tree. Any change to key order, indentation, or values that
  is not made through `sops` invalidates it, and decryption then fails.
- **The decryption key is not in the repo.** `clusters/production/apps.yaml`
  points `spec.decryption.secretRef` at a cluster Secret named `sops-age`; that
  private `age` identity exists only in the cluster.
- **`.enc.yaml` is a naming convention, not a guarantee.** The `.sops.yaml`
  rule keys encryption to the filename suffix; a mis-named file would be
  committed in cleartext, and nothing in the file's *content* announces it as
  the plaintext of an intended-encrypted resource.
- **All ciphertext here is fake.** Every `ENC[...]` string, the armored age
  block, and the `mac` are placeholders, not real SOPS output.

## Open questions

- If a tool must reflect a change to a resource whose `spec`/`data` it cannot
  read, what does it edit? It can see the object's identity but not its current
  values.
- Does re-serialising `secret.enc.yaml` (even reformatting whitespace or
  reordering keys) destroy the `mac` and make the file undecryptable — and how
  would a tool that rewrites YAML avoid that?
- Is the `sops:` block part of the "desired state" of the Kubernetes object, or
  is it out-of-band metadata that a manifest-aware tool should treat as opaque
  and never touch?
- To change one value under `data`, must a tool hold the private `age` key,
  decrypt, edit, and re-encrypt — and if it does not have the key, is any
  in-place edit of that value possible at all?
- How should a tool decide that `secret.enc.yaml` is encrypted: by the `.enc`
  suffix, by the presence of a top-level `sops:` key, by `ENC[...]` value
  shapes, or by matching `.sops.yaml`? Do those signals ever disagree?
- The `metadata` is cleartext but the referenced envFrom Secret's contents are
  not. Can a tool reason about what the running Pod actually consumes without
  ever decrypting?
