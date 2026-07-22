# NetworkPolicies (belt-and-suspenders, NOT applied by default)

These manifests are the **second layer** of containment. The **primary** control
in Wren is the in-pod iptables uid-lockdown (`egress-lockdown` init container,
WS-1 / spec §5.6): because the runner and the egress-proxy share a pod network
namespace, only a uid-owner match can distinguish them, and NetworkPolicy
operates at the pod (not container) level — so NetworkPolicy alone **cannot**
stop the runner from bypassing the proxy inside the pod.

What these policies add:

**Egress policies**

- `default-deny-egress.yaml` — a default-deny for agent pod egress, with a hole
  only for DNS (so the egress-proxy can resolve). This bounds egress at the
  *pod* boundary as a backstop if the in-pod lockdown is disabled or
  misconfigured. ⚠️ **Applied alone, this breaks runs** — the proxy and runner
  share a pod netns, so the deny hits the proxy too. Pair it with an allow
  policy for the proxy's upstreams (`fqdn-allow-cilium.yaml` or a port-443
  allow on plain-NetworkPolicy clusters).
- `fqdn-allow-cilium.yaml` — companion allow. A Cilium `CiliumNetworkPolicy`
  using `toFQDNs` to permit egress only to the known upstreams (github.com,
  api.github.com, api.anthropic.com). Requires Cilium / GKE Dataplane V2;
  plain Kubernetes NetworkPolicy has no FQDN matching. Adjust the label
  selector and FQDNs per deployment.

**Ingress policies**

- `default-deny-ingress.yaml` — denies all inbound traffic to agent pods. The
  iptables egress-lockdown governs OUTPUT only; replies on established flows are
  permitted (kubelet probes, future in-cluster streams). A connection initiated
  *into* the pod would give the runner a bidirectional channel the lockdown
  doesn't cut. Agent pods accept no inbound traffic by design today, so this
  policy closes that channel at zero functional cost. Revisit at M2 when the
  agent-gateway begins accepting control-plane steering connections.

**These are not applied by `config/default`.** Apply per run-namespace after
reviewing the selectors:

    # Egress containment (apply TOGETHER — egress deny alone breaks the proxy)
    kubectl apply -f config/netpol/default-deny-egress.yaml  -n <run-namespace>
    kubectl apply -f config/netpol/fqdn-allow-cilium.yaml    -n <run-namespace>

    # Ingress containment (safe to apply independently)
    kubectl apply -f config/netpol/default-deny-ingress.yaml -n <run-namespace>

The label selector targets agent pods via `wren.dev/component: agent`.
