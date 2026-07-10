# Publish age recipients as metadata

> Status: idea, not started.
> Context: the SOPS write path is already public-recipient only (see
> [`../security-model.md`](../security-model.md)). This removes the last reason to read the
> age-key Secret's `data` on a hot path.

## The idea

For a **generated** age key the controller already writes `configbutler.ai/age-recipient` on the
Secret. For a **BYO** age-key Secret it re-reads `data` to derive the recipient each time it needs
it, even though only the public half is ever used.

Derive the recipient once, publish it as a controller-owned annotation
(`configbutler.ai/age-recipients`), and let the write path read recipients from the `GitTarget`
spec plus that annotation. The private key `data` is then read only at bootstrap and rotation.

## The security rule that makes or breaks it

**Never trust a user-written recipient annotation.** If an attacker can set it to their own public
key, every Secret mirrored under that `GitTarget` is encrypted to a key they hold — a silent,
complete compromise of the encrypted-at-rest guarantee, with no error anywhere.

So exactly one of:

- the controller derives the annotation from the real key material and **overwrites** it on every
  reconcile, treating any other value as untrusted; or
- the `GitTarget` spec carries the public recipient directly, and the annotation is a cache keyed
  by the Secret's `resourceVersion`.

Anything that reads a recipient it did not itself derive is wrong.

## Why it is not urgent

Reading `data` to take a public half is not a leak — the value never leaves the process and never
reaches `sops`. This is a hot-path and blast-radius improvement, not a fix.
