# Idea: Commit signing key validation against the git platform

## Summary

A future version of gitops-reverser could query the git platform and report whether the public key
in `.status.signingPublicKey` is already registered on the remote account that is expected to sign
commits.

This is intentionally not part of the first commit-signing implementation.

---

## Why this is deferred

The current API does not ask for a platform account identifier such as:

- GitHub username or user ID
- GitLab username or account handle
- Gitea username

Today we only ask for committer name and email in `spec.commit.committer`. That is not enough to
reliably determine which remote account the operator should query.

Even if the platform exposes the right API, the operator would still need:

1. enough identity information to know which account to look at
2. platform credentials with permission to read key metadata
3. platform-specific client logic

Because of that, remote validation should be treated as a follow-on feature rather than part of the
initial SSH-signing rollout.

---

## What a future check would try to answer

The useful question is usually:

"Is the public key we are using for commit signing registered on the expected remote account?"

That is narrower than:

"Will the next commit definitely show as Verified?"

The second question is harder, because commit verification also depends on committer email and
platform-specific rules.

---

## Platform notes

### GitHub

GitHub has dedicated SSH signing key APIs, including:

- `GET /user/ssh_signing_keys`
- `GET /users/{username}/ssh_signing_keys`

So GitHub is the clearest candidate for a future integration. The main missing input in the current
API is the GitHub account identity to query.

### GitLab

GitLab exposes SSH key APIs and includes `usage_type`, which can distinguish signing-capable keys
from auth-only keys.

That means a future integration could likely answer:

- is this SSH key present on the account?
- is it allowed for signing?

But it still needs an account identifier and GitLab credentials.

### Gitea

Gitea supports SSH commit verification and exposes generic public-key management APIs, but it does
not appear to have a separate user-facing signing-key registration model distinct from ordinary SSH
public keys.

Inference:

- checking whether a key is registered is probably feasible
- checking whether it is specifically a "signing key" is less meaningful on Gitea than on GitHub or
  GitLab

---

## Possible future API shape

If this is revisited later, it probably needs explicit account identity, for example:

```yaml
spec:
  commit:
    signing:
      verification:
        provider: github
        username: gitops-reverser-bot
```

That should be designed separately from the initial signing rollout, because it introduces new
security and UX tradeoffs.

---

## Recommendation

Do not implement remote key validation in the first pass.

The first pass should:

1. generate or load the signing key
2. surface `.status.signingPublicKey`
3. document how the user registers that key on the platform

Once that is stable, revisit whether platform-account identity belongs in the API.
