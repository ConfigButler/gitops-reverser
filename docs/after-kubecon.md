* It's more than time to switch to audit event only (and use watchrules for initial cluster sync).
* I've noticed that make test-e2e is not always retriggering an image build when needed -> test this and see if we can fix it, that doesnt help.
* Can we also have more filtering on comitting of CRDs themself? At this moment you can only get them all, which feels a bit useless
* What do we do with namespaces? Make it a setting? Or just trust that people will do the right trickery to get rid of them when needed? Could make a nice setting on GitTarget
* In e2e test I see that CRD sync (of 61 resources) is not entirery happening in one commit -> the first handfull is sneaking in as seperate create commits.
