# CI container user: why we run as root

CI containers run as root. This is intentional and follows GitHub Actions and Docker conventions.

**Why it's fine:**
- CI containers are ephemeral and isolated — destroyed after each job
- GitHub Actions provides runner-level isolation
- Root is needed for tool installation, package managers, and Docker socket access
- The security boundary that matters is the production runtime, not the build environment

**Production already does the right thing:**
The release image uses `gcr.io/distroless/static:nonroot` with `USER 65532:65532`. That's
where privilege matters — not in the build/test container.

**When to reconsider:** only if a compliance framework explicitly requires it (SOC2, PCI-DSS)
or you're running self-hosted runners without proper isolation.

## References

- [GitHub Actions: Security hardening](https://docs.github.com/en/actions/security-guides/security-hardening-for-github-actions)
- [Docker: Build and runtime security best practices](https://docs.docker.com/build/building/best-practices/#user)
