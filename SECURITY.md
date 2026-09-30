# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately via
[GitHub Security Advisories](https://github.com/truvity/github-structure/security/advisories/new).

Do NOT open a public issue for security vulnerabilities.

## Supported Versions

Only the latest release is supported with security updates.

## What is in scope

This repository publishes:

- The Go packages `pkg/registry`, `pkg/engine`, `pkg/preflight` and `pkg/app`.
- The documentation, where it tells an adopter to configure an organisation in an unsafe way.

Reports that matter most:

- A registry or preflight that accepts a configuration it should refuse: a branch protection or ruleset that silently does less than it says, a bypass actor that is wider than declared, a plan that destroys something it promised to guard.
- `pkg/app` leaking an App key, installation token or JWT into a log line, an error or a returned value, or acting on an installation other than the one named.
- A default that weakens what the registry enforces on an organisation.

A finding that depends on how a particular deployment uses this repository
belongs with that deployment's owner.
