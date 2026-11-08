# Security Policy

## Supported versions

Only the latest minor release receives security fixes.

## Reporting a vulnerability

Do not open a public issue. Report privately through
[GitHub Security Advisories](https://github.com/murasame29/workload-knocker/security/advisories/new)
of this repository.

## Known limitations

`/api/v1` has no authentication in versions before the one that adds auth. Restrict network
access to the Service with a NetworkPolicy or an authenticating proxy, and do not expose it
publicly.
