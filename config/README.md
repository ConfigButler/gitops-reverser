# config_raw

This folder is a static, rendered snapshot of `kustomize build config/default`.

Goals:
- Keep manifests simple and explicit.
- Avoid patches/replacements/transformer indirection.
- Make side-by-side comparison with `config/` easy.

Notes:
- These files are intentionally environment-specific to the current render profile.
- Update by re-rendering from `config/default` when source config changes.
