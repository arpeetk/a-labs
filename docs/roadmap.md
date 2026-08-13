# Roadmap

This file lists product gaps that are intentional and still current. It is not
a delivery ledger; use GitHub issues for ownership, sequencing, and discussion.

## Security and identity

- Replace the trusted `X-Wren-User` header with an authenticated OIDC/SSO
  front door.
- Replace the shared GitHub PAT with short-lived, repository-scoped GitHub App
  installation tokens minted per run.
- Add isolated agent node pools and evaluate gVisor or Kata for kernel-level
  isolation.

## Durability and operations

- Provision and manage Cloud SQL lifecycle instead of only supplying the HA
  deployment overlay.
- Add aggregated historical logs and multi-attempt log selection.
- Add incremental or git-aware checkpoints, final/termination snapshots, and
  transcript continuity for every harness.
- Add automatic stuck-run and budget watchdogs.

## Product surface

- Add interactive attach/steer and MCP management only when their backing
  services are complete; do not add placeholder CLI commands.
- Add project editing, usage/cost views, and policy/budget administration.
- Replace HTTP/JSON with a versioned Connect/gRPC API when compatibility needs
  justify the migration.
- Add a GitOps-oriented packaging path if the CLI installer is insufficient
  for production operators.

## Validation gaps

- Run OpenCode against a live provider; its adapter and image currently have
  hermetic coverage only.
- Exercise the release workflow on an actual version tag.
- Validate restricted-admission behavior on environments that reject the
  privileged egress-lockdown init container.
