# Security Policy

## Supported versions

Only the latest minor release receives security fixes.

## Reporting a vulnerability

Do not open a public issue. Report privately through
[GitHub Security Advisories](https://github.com/murasame29/workload-knocker/security/advisories/new)
of this repository.

## Known limitations

`/api/v1` is protected by a single shared bearer token (`API_TOKEN`) with no per-user identity
or rotation support. Restrict network access to the Service with a NetworkPolicy and do not
expose it publicly.

Without an authorization policy file (`AUTHZ_CONFIG_PATH`), every chat user and the API caller may
start and stop every workload. Enable a policy for shared environments; the policy has allow rules
only, so keep them narrow and `default: deny`. See
[docs/en/configuration.md](docs/en/configuration.md#authorization).
