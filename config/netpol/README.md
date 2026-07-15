# Egress NetworkPolicies (belt-and-suspenders, NOT applied by default)

These manifests are the **second layer** of egress containment. The **primary**
control in Wren is the in-pod iptables uid-lockdown (`egress-lockdown` init
container, WS-1 / spec §5.6): because the runner and the egress-proxy share a
pod network namespace, only a uid-owner match can distinguish them, and
NetworkPolicy operates at the pod (not container) level — so NetworkPolicy alone
**cannot** stop the runner from bypassing the proxy inside the pod.

What these policies add:

- `default-deny-egress.yaml` — a namespace-wide default-deny for pod egress,
  punching a hole only for DNS (so the egress-proxy can resolve) and for the
  cluster's own needs. This bounds egress at the *pod* boundary as a backstop if
  the in-pod lockdown is ever disabled or misconfigured. It cannot express
  per-container rules, so it is coarser than the iptables layer.
- `fqdn-allow-cilium.yaml` — an **example** Cilium `CiliumNetworkPolicy` using
  `toFQDNs` to allow egress only to the known upstreams (github.com,
  api.github.com, api.anthropic.com). Requires Cilium / GKE Dataplane V2;
  plain Kubernetes NetworkPolicy has no FQDN matching. Adjust the label
  selector and FQDNs per deployment.

**These are not applied by `config/default`.** Apply them per run-namespace
after reviewing the selectors:

    kubectl apply -f config/netpol/default-deny-egress.yaml -n <run-namespace>

The label selector targets agent pods via `wren.dev/component: agent`.
