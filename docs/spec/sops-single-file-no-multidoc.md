# SOPS and multi-document YAML: single-file decision

> Status: decided
> Captured: 2026-06-08
> Related:
> [file-agnostic-placement.md](gittarget-new-file-placement-rules.md),
> [contextual-namespace-and-kustomize-folder-editing.md](contextual-namespace-and-kustomize-folder-editing.md),
> [../sops-repo-bootstrap-and-key-management-architecture.md](../finished/sops-repo-bootstrap-and-key-management-architecture.md),
> [../sops-repo-bootstrap-out-of-scope.md](../finished/sops-repo-bootstrap-out-of-scope.md)

## Decision

For SOPS-encrypted content, gitops-reverser keeps **one Kubernetes resource per
SOPS file**. We do **not** write SOPS-encrypted multi-document YAML (no
`\n---\n`-separated documents inside an encrypted file).

Plaintext manifests may still be multi-document where that is convenient (see
[file-agnostic-placement.md](gittarget-new-file-placement-rules.md)); this decision is
scoped to files we encrypt with SOPS.

## Why this came up

The file-agnostic placement work allows multiple resources in one YAML file via
the `---` separator. The natural question was whether the same trick is usable
for SOPS-encrypted files — i.e. can we keep several encrypted resources in one
file and still edit/replace them independently, the way we edit one resource
without disturbing its neighbours. The answer changes how the writer must treat
encrypted files, so it is worth recording.

The findings below are from reading the upstream SOPS sources (`getsops/sops`,
cloned locally under `external-sources/sops`, which is gitignored — paths below
reference upstream packages, not that local copy).

## How SOPS handles multi-document YAML

Multi-document YAML *is* a first-class, tested feature of the SOPS YAML store
(`stores/yaml/store.go`). The store is built around `sops.TreeBranches` — a
slice of branches — where each `---`-separated document is one branch.

- **Load** (`LoadPlainFile`): a decoder loop calls `Decode` until `io.EOF`,
  turning each document into its own branch.
- **Emit** (`EmitPlainFile`): loops over the branches and encodes each as a
  separate document node, producing `---`-delimited output.

Constraints in the store: each document root must be a **mapping**; a top-level
sequence or scalar document is rejected, and an empty/`null` document yields an
empty branch.

## Why the documents are cryptographically one unit

Even though the documents are physically separate, SOPS binds them together:

1. **One data key for the whole file.** `Tree.Encrypt(key, cipher)` runs once
   over the entire tree (all branches) with a single data key.

2. **One file-wide MAC over all documents.** In `sops.go`, `Tree.Encrypt`
   creates a single `sha512` hash and folds **every** branch into it, in order:

   ```go
   hash := sha512.New()
   ...
   for _, branch := range tree.Branches { // every document
       walk(branch)                       // hash.Write(...) per value
   }
   return fmt.Sprintf("%X", hash.Sum(nil)) // one MAC for the file
   ```

   On decrypt, SOPS recomputes that MAC over all branches and compares it to the
   stored value. Changing any value in any document changes the file-wide MAC; a
   stale MAC fails with `MacMismatch` (unless `--ignore-mac`).

3. **The same metadata (including that one MAC) is written into every
   document.** `SerializeMetadata` appends the `sops:` block to every branch on
   emit. On load, `ExtractMetadata` reads the `sops:` block **only from document
   0** (`if bi == 0`) and **strips** the `sops:` key from every other document
   unconditionally.

Consequence: you cannot replace or swap a single encrypted document in place.
Any change forces a re-MAC over the whole tree, i.e. a whole-file re-encrypt.

## Why the "roll your own multi-doc" trick does not work

The tempting workaround is to SOPS-encrypt each document independently and
concatenate them with `---`. Stock `sops decrypt` cannot read that:

- It reads metadata only from document 0 and strips the `sops:` key from the
  rest (`ExtractMetadata`).
- It then tries to decrypt documents 1..N with document 0's data key and verify
  against document 0's MAC, which was computed over all branches concatenated.
- Different data key → AES-GCM auth failure; same data key → MAC mismatch.
  Either way decryption fails.

It is only viable if *we* own the decrypt path and split on `---` first, handing
each standalone document to SOPS individually. That is not how the encrypted
files are consumed downstream (Flux / sops-aware tooling expect the canonical
single-MAC format), so it is a dead end for us.

## The deeper constraint: editing needs the data key

The real reason we want to avoid re-encryption is to avoid needing the data key
at write time. That goal is incompatible with in-place editing of *any* SOPS
document, multi-doc or not, because **the MAC is stored encrypted under the data
key**. Even a one-value change forces a MAC update, and updating the MAC
requires the data key. So "edit without re-encrypt" reduces to "edit without the
key", which SOPS does not allow for an in-place change.

(Aside: the IV stash in `aes/cipher.go` reuses the IV for unchanged
`(plaintext, additionalData)` pairs within a single decrypt→edit→encrypt cycle,
so unchanged values keep byte-identical ciphertext and the git diff stays
minimal. That keeps re-encrypt cheap and low-churn — but it still re-MACs, and
the additional data is the in-document key path with no document index, so it
does not give per-document independence.)

## The trilemma

You can have any two of the following, not all three:

| Want                                   | Cost                                                        |
| -------------------------------------- | ----------------------------------------------------------- |
| Single file + stock `sops decrypt`     | Must re-MAC on any change → need the key, no partial edits   |
| Single file + no re-MAC                | Custom decrypt path (split on `---` first, decrypt per doc)  |
| Stock `sops decrypt` + no re-MAC       | Multiple files — one SOPS document per file                  |

Per-resource independence (add / replace / drop a resource as a pure file
operation, no data key, no re-MAC) is only available with **one SOPS document
per file** — the third row.

## Rationale for the decision

- We want encrypted resources to be independently placeable and replaceable by
  the manifest writer without holding decryption keys, matching how plaintext
  resources are edited by content identity.
- The single multi-doc file is exactly the construct that couples resources
  through one shared MAC, defeating that.
- Downstream consumers expect canonical SOPS files; the roll-your-own format
  would not decrypt.

One SOPS document per file gives us the property we want with no custom crypto
and full compatibility with stock SOPS and Flux.

## Implications for gitops-reverser

- When the writer encrypts a resource, it writes that resource to its own file;
  it never appends an encrypted resource as an extra `---` document.
- Placement logic for encrypted resources must therefore not co-locate multiple
  resources in a single encrypted file, even where the plaintext placement rules
  would allow a multi-doc file.
- Re-encrypting a single-resource file is acceptable and expected when that
  resource's content changes; it is not the thing we are avoiding. What we avoid
  is one resource's change forcing a re-encrypt that touches *other* resources.

## Out of scope

- Changing or forking SOPS multi-doc handling.
- A custom split-then-decrypt path owned by gitops-reverser.
- Any change to how plaintext multi-document YAML is parsed or written.
