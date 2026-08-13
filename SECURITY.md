# Security policy

Wren executes untrusted, model-generated code. Security is therefore a system
contract, not an optional deployment feature. The detailed design is in
[`docs/technical-spec.md`](docs/technical-spec.md); this file is the concise,
operator-facing threat model and residual-risk statement.

## Report a vulnerability

Do not open a public issue. Use the repository's **Security → Report a
vulnerability** flow (preferred), or email **arpeetkale@gmail.com** with a
subject beginning `[SECURITY]`.

Include the affected commit or version, reproduction steps or a proof of
concept, and the expected impact. We aim to acknowledge reports within 72
hours and coordinate disclosure with the reporter. Wren is pre-1.0 and has no
formal supported-version matrix; fixes currently land on `main`.

## Trust boundaries

The harness container, the model, repository content, and any code or tools the
harness invokes are untrusted. The egress proxy, hydrate/checkpoint containers,
operator, and control plane are trusted components. Some trusted containers
share a pod network namespace or workspace volume with the harness, so their
interfaces are deliberately narrow:

- the harness runs non-root with a read-only root filesystem, dropped
  capabilities, seccomp, no privilege escalation, and no service-account
  token;
- the workspace PVC is the harness's only writable durable filesystem;
- checkpoint storage and its Workload Identity are mounted only into trusted
  hydrate/checkpoint containers;
- GitHub and model-provider credentials exist only in the egress proxy;
- the runner can reach only the local proxy when iptables enforcement is on;
- output is a branch and pull request—the agent cannot merge or deploy through
  Wren itself.

These controls limit a compromised harness; they do not make `runc` a
kernel-security boundary or make broadly scoped credentials least-privilege.

## Current controls and residual risks

### Egress containment

The default `iptables` mode runs a privileged `egress-lockdown` init container
that installs IPv4 and IPv6 OUTPUT rules. The harness uid can reach only the
in-pod proxy; the proxy uid may reach the network. Every enforced run executes
a startup canary that fails if direct egress succeeds, and records an
`EgressEnforcement=True` condition.

`--egress-enforcement=off` removes that containment boundary. It exists for
clusters that reject the privileged init container, including GKE Autopilot
and restricted Pod Security Admission namespaces. In this mode the harness can
bypass the proxy; the run records `EgressEnforcement=Disabled`. Use the
policies under [`config/netpol/`](config/netpol/) as defense in depth, ideally
with an FQDN-aware CNI, and do not describe this mode as contained.

The lockdown does not block new inbound connections. Agent pods expose no
service by design, but operators should still apply default-deny ingress in run
namespaces.

### Credentials

The harness cannot read raw GitHub, Anthropic, or OpenAI credentials. The proxy
strips caller-supplied auth and injects credentials only on explicit upstream
routes. A compromised harness can nevertheless *use* those routes, so the
credential's external scope remains part of the blast radius.

Today GitHub authentication is a shared PAT, not a short-lived repository-
scoped GitHub App token. Give it only the repository permissions required to
clone, push a Wren branch, and open a pull request. Per-run GitHub App token
minting remains roadmap work.

### Control-plane authentication

The public API trusts the `X-Wren-User` header; it does not cryptographically
authenticate callers. Keep the API on a port-forward or a private, trusted
network with an authenticated gateway in front of it. Do not expose it
directly to the public internet. OIDC/SSO is required before treating Wren as a
secure multi-user service.

The internal event-ingest route is separate: the gateway reaches it through
the proxy, which injects a shared internal token and fixes the run identity.

### Container and cluster isolation

Agent pods currently use `runc`. Non-root execution and pod hardening reduce
attack surface but do not contain a kernel exploit. Higher-assurance
deployments should use a dedicated agent node pool with minimal node IAM.
gVisor/Kata integration remains roadmap work.

Run namespaces are configurable and may be shared; Wren does not currently
enforce namespace-per-user isolation. Each run does receive its own PVC,
ConfigMap, pod, and AgentRun object. Kubernetes RBAC, namespace policy, quotas,
and node placement remain operator responsibilities.

### Durability and availability

The default in-memory store is for evaluation. It re-learns AgentRun state from
the cluster after restart, but project configuration and completed history are
not durable. The Postgres store provides migrations, transactional launch
intent, leased outbox replay, durable events, and readiness checks; operators
must still provision, back up, and monitor Postgres or Cloud SQL.

Workspace PVC reattachment handles ordinary pod replacement. With a configured
checkpoint mount, Wren publishes verified full-workspace snapshots, restores
after confirmed PVC loss, and supports durable pause/resume from an exact
checkpoint. Without a mount, destroyed PVC data is unrecoverable and the run
fails `WorkspaceLost`. Snapshots are periodic full archives, so work since the
last successful checkpoint can still be lost.

### Remaining product controls

Automatic token/cost budgets, per-user quotas, authenticated authorization,
and comprehensive audit policy are not yet enforcement features. Image
signing, provenance verification, and admission policy are also deployment
responsibilities today. Track intentional gaps in
[`docs/roadmap.md`](docs/roadmap.md).

## Operator hardening checklist

- Keep the API private or put an authenticated gateway in front of it.
- Keep egress enforcement on; treat `off` as a documented security downgrade.
- Scope GitHub and provider keys as narrowly as their services allow.
- Use a dedicated agent node pool with minimal node credentials.
- Apply namespace-level default-deny ingress and defense-in-depth egress policy.
- Use Postgres with backups for durable control-plane state.
- Configure and monitor checkpoint storage for recoverable workspaces.
- Treat every repository and MCP response as adversarial prompt content.
