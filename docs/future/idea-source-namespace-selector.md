# A namespace selector in a `KubeProvider` grant's scope

Status: **idea**, nothing built. Recorded from the review of the grant shapes that #441 shipped.
A grant's consumer side (`consumerNamespaces`) already takes a selector; this page is about the
scope side, the source-cluster namespaces a grant lets a `GitTarget` mirror (`sourceNamespaces`).

Should a platform-owned `KubeProvider` be able to grant "namespaces labeled
`platform.example.com/tenant: team-a`" in the source cluster? Then a tenant's newly provisioned
namespaces would be covered as soon as they carry the label, without anyone editing the grant. That
is a reasonable capability. It is not built; this page records where it would fit.

**The September removal priced the work; it did not judge the design.**
[source-scope-simplification.md](../design/source-scope-simplification.md) deleted
`GitTarget.spec.allowedSourceNamespaces` and its source-side selector on 2026-09-01, about 4,500
lines. Part of its argument no longer holds: it called the field "cheap only because it enforces
nothing", because the tenant wrote it on their own `GitTarget`. Grants now sit on the
platform-owned `KubeProvider`, so a selector there would enforce something.

**Two capabilities, to be kept apart:**

- **Authorization.** A selector in the grant admits namespaces that a WatchRule still names
  explicitly. That is useful on its own: the platform manages labels instead of grant lists. It is
  also the smaller piece, because the scope check reads the labels of a namespace that is already
  named.
- **Automatic following** ("watch everything my scope selects"). Today a `"*"` rule is refused
  under a narrower scope, never narrowed to it, and a `WatchRule` item otherwise names one namespace. So this needs
  its own rule-side design, such as a `"*"` that is narrowed instead of refused. That would change
  the meaning of a value users already write, so it needs its own decision. It is later work, and
  not a precondition for authorization.

**The operational contract to settle before building either:**

- revocation when a label is removed, mirroring
  `TestGitBranch_ANamespaceLabelLossRevokesASelectorGrant`
  (`internal/controller/gitbranch_access_test.go`) on the control side;
- what happens when source-cluster labels cannot be read: a failed lookup must never become an
  empty selection or a silent allow, and it is not a denial either. The deleted three-valued
  verdict existed for this;
- who may write authorization labels in the source cluster, where a tenant's own Flux or Argo CD
  may be able to label Namespaces;
- `Namespace` get/list/watch for the provider's identity.

**Shape, when it comes:**

- **Add a sibling field, `sourceNamespaceSelector`, next to the `sourceNamespaces` list.** It is
  additive: every manifest written today stays valid, which is what justifies keeping
  `sourceNamespaces` a list now. The alternative, turning it into an object
  (`sourceNamespaces: {names: [...], selector: ...}`), would break every existing manifest. Once
  there are users, that needs a migration or a new API version. That cost decides it, not
  aesthetics. Nesting it now only to avoid that later would cost every reader today, for a field
  that doesn't exist.
- **Make the two arms alternatives, not additions.** In `consumerNamespaces`, `names` and
  `selector` are alternatives picked by `from`. A grant with both `sourceNamespaces` and
  `sourceNamespaceSelector` should be refused, so the same two ideas keep one combination rule in
  one grant. Users get the union through separate grants, as they already do for consumers ("Team
  B or every tenant is two grants").
- **Don't reuse the `from` enum.** `Same` means the holder's control-cluster namespace and has no
  meaning in the provider's cluster. Exact names, `"*"` and `{consumer.namespace}` already cover
  the useful cases, and `"*"` stays in the list, where it is today.
- **Consider `{consumer.namespace}` in label values**
  (`matchLabels: {platform.example.com/tenant: "{consumer.namespace}"}`). Static selectors already
  pay for themselves: a tenant's newly labeled namespaces are authorized without editing its
  grant. The variable adds one thing on top: one grant can serve every tenant, instead of one
  grant per tenant. Useful, but not what makes selectors worthwhile.
- **`applierNamespaces` gets no selector** until someone asks for one. Appliers live in a few
  fixed namespaces such as `flux-system`.
