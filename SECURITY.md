# Security Policy

## Supported Versions

Squall is pre-release. No version is supported for security patches yet; the table below
becomes real at the first tagged release.

| Version | Supported |
| ------- | --------- |
| 0.1.x   | pre-release, not yet supported |

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Use GitHub's [private vulnerability reporting](https://github.com/ackstorm/squall/security/advisories/new)
to open a draft security advisory. It reaches the maintainers privately and keeps the
disclosure coordinated.

### What to include

- Type of issue (e.g. credential exposure, privilege escalation, SSRF, injection)
- Full paths of the source files involved
- The affected revision (tag, branch or commit)
- Any configuration required to reproduce
- Step-by-step reproduction instructions
- Proof-of-concept or exploit code, if you have it
- Impact — how an attacker would use this

### Response timeline

- **Initial response**: within 48 hours of the report
- **Status updates**: at least every 7 days while the issue is open
- **Resolution**: we aim to resolve critical vulnerabilities within 30 days
- **Disclosure**: coordinated, 90 days from initial report by default

We will acknowledge your contribution in the advisory unless you prefer to remain
anonymous.

## Threat model

Squall provisions compute on third-party GPU marketplaces and moves inference traffic to
it. Its security posture is documented in the specification under `docs/specs/`, including
the accepted residual risks. Read that before reporting an issue about the provider
boundary — some properties are deliberate, documented trade-offs rather than defects.

Two things are worth stating here because they shape every report:

- **No public HTTP ingress.** The client → LiteLLM → proxy → gateway path is private.
  Provider transport is either routed-private or an outbound SSH tunnel initiated from
  the provisioned host.
- **Kubernetes is not in the serving compute.** The cluster holds intent and coordination
  only, so cluster-level isolation does not protect the serving replica.

## Deployment recommendations

- **Least privilege**: run the controller with the minimal RBAC it ships with; do not
  grant cluster-admin.
- **Network policy**: restrict controller and proxy egress to the dstack server and
  gateway.
- **Secrets**: provider credentials and the dstack token belong in Kubernetes Secrets or
  an external secret manager, never in a `Model` CR.
- **Updates**: track releases; provider integrations change under us.
- **Monitoring**: watch controller logs for repeated provisioning failures — they are
  the first sign of a compromised or misbehaving provider account.
