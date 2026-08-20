---
title: Per-Sandbox Endpoint Addressing
authors:
  - "@BSWANG"
reviewers:
  - "@AiRanthem"
  - "@chengzhycn"
creation-date: 2026-08-13
last-updated: 2026-08-20
status: provisional
see-also:
  - "/docs/proposals/20260527-dynamic-sandbox-domain.md"
  - "/docs/proposals/20260711-short-sandbox-id.md"
  - "/docs/proposals/20260813-sandbox-l7-endpoint-addressing-zh_CN.md"
---

# Per-Sandbox Endpoint Addressing

## Summary

Pod IP is the only way to reach a Sandbox today, and every component assumes it is
present and usable. Neither holds in general: the IP may be a placeholder such as
`169.254.1.1`, or the Pod network may be unroutable from the caller.

This proposal adds `status.endpoint`, parallel to `status.sandboxIp`, declaring how a
given Sandbox is reached: `Direct` (the Pod IP, as today) or `Hostname` (a routable
name served by an L7 front). **A cluster may mix both, per Sandbox** — which is why
this belongs on the object: the mode and the domain vary per Sandbox, so no
process-level rule has a correct value.

The endpoint carries the whole recipe — address, authority, path prefix, headers and
a credential reference — so no component needs new flags and both ends cannot
disagree.

The field is derived, not configured: the addressing rule belongs to the backend a
Sandbox runs on, and whatever creates Sandboxes there already knows how they are
reached. So there is no user-facing knob — no annotation, no spec field.

**Who writes it is deliberately left out of this iteration.** The field defaults to nil,
so every Sandbox stays `Direct` until something sets it: a later controller extension,
or a deployment's own webhook or operator that already knows its L7 front. What this
proposal fixes is the field, the resolver and the consumers — the part that must not be
reimplemented once per site.

A nil `status.endpoint` means `Direct`, so existing Sandboxes and a rolled-back
controller keep working unchanged.

## Motivation

- **The IP may be a placeholder.** Some deployments report a fixed link-local address
  for every Sandbox and decide the real destination from L7 information on the path.
- **The Pod network may be unroutable** from the components that manage and proxy
  Sandboxes — another cluster, VPC or network zone, or a Pod network exposed only
  through a managed L7 load balancer.
- **A cluster may hold both kinds at once**, which forces this to be per-Sandbox
  rather than per-process.

### Goals

- Make per-Sandbox addressing an explicit, observable attribute.
- Let `Direct` and `Hostname` Sandboxes coexist, decided per Sandbox at every site.
- Add no flags: the object carries everything needed to reach the Sandbox.
- Add no user-facing API surface for a decision no user makes: no annotation and no
  spec field.
- Keep one contract for every deployment — one field, one resolver, one readiness
  rule — whoever writes the field.
- One resolver for every site, so the control plane and the two data planes cannot
  drift.
- Unchanged behavior for Sandboxes that declare nothing.
- Reuse the existing `e2b-sandbox-id` / `e2b-sandbox-port`,
  `{port}-{sandboxID}.{domain}` and `/kruise/{sandboxID}/{port}` conventions.

### Non-Goals

- **Federating a foreign Sandbox backend** (e.g. a real E2B cluster behind this
  control plane). The endpoint attribute is the seam that would make those
  expressible; their lifecycle is a separate proposal.
- **A declarative intent API.** No annotation and no spec field selects `Hostname`.
  The mode follows from the backend the controller placed the Sandbox on, which the
  controller already knows, so a knob would expose a decision no user makes — and
  which end users would nonetheless see on their own Sandboxes.
- **The writer.** Choosing which Sandbox is `Hostname` and rendering its address is
  out of scope here. The field stays nil unless something sets it — a controller
  extension or a status webhook, once a deployment has a front to point at.
- Programming the L7 front: listeners, rules, certificates and DNS are the
  deployment's responsibility.
- Removing `status.sandboxIp` or the `ReturnPodIP` extension; changing the token
  model; multi-cluster discovery; reverse tunnels.

## Current State

Five sites resolve a Sandbox to `<PodIP>:<port>`:

| # | Site | Where |
|---|---|---|
| A | Runtime client, plaintext | `pkg/utils/runtime/runtime.go:44` |
| A' | Runtime client, TLS — fixed authority, dial pinned to the Pod IP | `client.go:283`, `:307`, `:321`, `transport.go:305` |
| B | CDP proxy | `pkg/utils/proxyutils/default.go:40` |
| C | Manager Envoy, `ORIGINAL_DST` via `x-envoy-original-dst-host` | `pkg/proxy/ext_proc.go:139` |
| D | Gateway Envoy, two `ORIGINAL_DST` clusters via `envoy.lb.original_dst`; the second re-encrypts the runtime port with mTLS | `pkg/sandbox-gateway/filter/filter.go:146`, `:148` |

A, A' and B are shared with `agent-sandbox-controller`, which dials Sandboxes in five
flows of its own: CSI mount and init handshake
(`controller/sandbox/core/sandbox_initializer.go:160`, `:186`), lifecycle hooks
(`lifecycle_handler.go:68`), recycle (`recycle.go:192`), token refresh
(`controller/securitytokenrefresh`). So the controller both writes the new attribute
and consumes it, and its init flows run during startup — a `Hostname` Sandbox it
cannot address never finishes initializing.

Two places publish a Pod IP outward instead of dialing it: the `ReturnPodIP` E2B
extension (`servers/e2b/sandbox.go:145-147`, the only non-test consumer of
`infra.Sandbox.GetIP()`) and `status.sandboxIp` itself.

Five places gate readiness on a non-empty Pod IP: `cache/tasks.go:128`,
`infra/sandboxcr/claim.go:555`, `:926`, `:970`, and `sandboxroute/route.go:98-102`,
which projects a Sandbox with no Pod IP as `creating`. A placeholder satisfies all of
them permanently; a `Hostname` Sandbox fails all of them.

Other Pod-IP users — peer gossip, route refresh between control-plane Pods, the
webhook self-check — are not Sandbox traffic and are unaffected.

## Design

### The attribute

```go
// Endpoint declares how this Sandbox is addressed. A nil Endpoint means
// Direct via status.podInfo.podIP.
// +optional
Endpoint *SandboxEndpoint `json:"endpoint,omitempty"`

type SandboxEndpoint struct {
    // Mode is the addressing mode: Direct or Hostname.
    Mode SandboxEndpointMode `json:"mode"`

    // Address is the host and port to connect to for this Sandbox, e.g.
    // sbx-alb.example.com:443. Required when Mode is Hostname.
    // +optional
    Address string `json:"address,omitempty"`

    // Scheme is http or https for the hop to Address. Defaults to https.
    // +optional
    Scheme string `json:"scheme,omitempty"`

    // Authority replaces the Host / :authority header, e.g.
    // {port}-sbx7f3a.sbx.example.com. Empty keeps Address as the authority.
    // +optional
    Authority string `json:"authority,omitempty"`

    // PathPrefix is prepended to the request path, e.g. /kruise/sbx7f3a/{port}.
    // +optional
    PathPrefix string `json:"pathPrefix,omitempty"`

    // Headers are sent with every request to this Sandbox, e.g.
    // {"e2b-sandbox-id": "sbx7f3a", "e2b-sandbox-port": "{port}"}.
    // +optional
    Headers map[string]string `json:"headers,omitempty"`

    // AuthSecretRef names the Secret supplying {token}. Empty means the front
    // needs no credential of its own.
    // +optional
    AuthSecretRef *corev1.LocalObjectReference `json:"authSecretRef,omitempty"`
}
```

`AuthSecretRef` is part of the target contract but is deferred from the current
addressing spike. The validated implementation covers fronts that require no
additional credential; Secret resolution, rotation and RBAC remain step 5.

At the top level of `status`, not inside `podInfo`: a `Hostname` Sandbox's address is
not a Pod property and `podInfo` may be empty for one.

**The endpoint carries the whole decoration, so no component takes new flags.** Only
two placeholders survive, because everything else is known when the field is written:
`{port}`, which the caller picks per call, and `{token}`, which comes from
`AuthSecretRef`. The Sandbox ID, the domain and every other part are concrete strings
by the time the field is written, and the resolver rejects any placeholder it does not
know.

This puts transport mechanics on a Kubernetes object, which is a real cost. It buys
three things: one place to configure instead of the same flags in three components,
no requirement that both ends agree, and per-Sandbox fronts for free — a cluster can
have Sandboxes behind different addresses, different header names and different
credentials at once.

A writer derives the field rather than reading intent, the way `status.sandboxIp` is
derived today: the mode follows from the backend the Sandbox runs on. **This iteration
writes nothing.** The field stays nil, every Sandbox resolves `Direct`, and the
addressing contract lands and gets consumed before anything starts producing `Hostname`
Sandboxes. A deployment that already has an L7 front can populate `status.endpoint`
itself — from a controller extension, or from its own webhook or operator — and inherits
this API, this resolver and every consumer unchanged.

That leaves a status field nothing here writes yet, which is this shape's honest cost.
The rejected alternative — an annotation or spec field carrying intent — costs more: a
knob on every Sandbox that no user sets, that end users see anyway, and that nobody has
asked for. Adding a writer later changes nothing for consumers.

Whatever ends up writing the field owes the consumers four things:

- **Concrete values.** `{port}` is the only placeholder that may survive into status.
  `Validate` rejects every other one, along with an unknown mode, a missing or
  malformed `Address` under `Hostname`, an invalid header name, and — critically — an
  endpoint that renders `{port}` nowhere, since every such request would land on the
  front's default backend port. Consumers validate again at resolution, and unknown
  modes fail closed rather than silently downgrading to `Direct`.
- **One patch.** Publish the endpoint in the same status update as the phase and
  `podInfo` it belongs to, so no reader pairs a new generation's phase with the
  previous generation's address.
- **Every path.** Set it on every path that reaches a running Sandbox, not only on
  creation. `calculateStatus` transitions on `box.Spec.Paused`
  (`sandbox_controller.go:648`, `:666`), so a phase jump can bypass a creation-only
  refresh.
- **Explicit revocation.** To stop a Sandbox from being addressable, write `Hostname`
  with an empty `Address` — never nil. Nil means `Direct`, which re-points every
  consumer at a Pod IP that is a placeholder under `Hostname`. Status is patched with
  `MergePatchType` (`sandbox_controller.go:516`) and `Endpoint` is `omitempty`, so
  omitting the field leaves the previous value in etcd rather than clearing it;
  removing it altogether takes an explicit JSON null, which marshalling the whole
  status cannot express.

Credentials follow `runtimecredentials`' key layout: `token` for a bearer or opaque
value, or `client.crt` / `client.key` / `ca.crt` for mTLS to the front. Each consumer
reads the Secret through an informer-backed cache, which also picks up rotation, and
passes the resolved string to the resolver so the leaf package stays free of
Kubernetes types. The gateway data plane resolves it in the Go filter and injects the
header, since Envoy cannot read a Secret per request. `Route.AccessToken` already
travels the same path under a never-log rule (`pkg/sandboxroute/AGENTS.md`).

The reference is namespace-less so that nothing outside the controller can aim a
control-plane component at an arbitrary Secret; which namespace it resolves in is
[open](#open-questions). With no intent API there is no tenant-supplied value to copy
in the first place — the controller derives the whole endpoint.

### Resolution

Every site asks one resolver, per request, from the Sandbox's own attribute. A nil
endpoint, an empty mode and explicit `Direct` return `podIP:port` and change nothing
about the request. `Hostname` connects to `Address`, whose port is fixed, so **the
target port cannot be expressed by the connection** and has to travel inside the
request — through `Authority`, `PathPrefix`, `Headers`, or any combination. Any
other mode is invalid, not a compatibility alias for `Direct`:

```
authority    GET https://3000-sbx7f3a.sbx.example.com/init
                 Host: 3000-sbx7f3a.sbx.example.com
                 endpoint.authority: {port}-sbx7f3a.sbx.example.com
path         GET https://sbx-alb.example.com/kruise/sbx7f3a/3000/init
                 front restores :path = /init
                 endpoint.pathPrefix: /kruise/sbx7f3a/{port}
header       GET https://sbx-alb.example.com/init
                 e2b-sandbox-id: sbx7f3a  e2b-sandbox-port: 3000
                 endpoint.headers: {e2b-sandbox-id: sbx7f3a,
                                    e2b-sandbox-port: "{port}"}

all three connect to endpoint.address = sbx-alb.example.com:443
```

All three shapes are conventions this repository already parses
(`native_e2b.go:45` and `:28-30`, `customized_e2b.go:34-62`), so a `sandbox-gateway`
acting as the front needs no new parsing code — and because the header names come
from the object, a front with different names needs no client change either.

None of the shapes requires authority to participate in DNS or certificate
verification: the rendered authority is only the Host / `:authority` value. The
connection URL still uses the front's `Address`, so the standard Go transport derives
TLS `ServerName` from `Address`. This is about the internal hop only, and should not
be confused with the client-facing wildcard ingress the deployment already ships
(`config/sandbox-manager/ingress.yaml:18`), which does need a wildcard certificate
because browsers resolve those names.

Because `Address` is per-Sandbox, the data planes cannot point a single static cluster
at one front. Both now carry two `dynamic_forward_proxy` clusters — one plaintext, one
TLS — selected per request by an internal header the Go filter sets, with the front's
host and port handed to the DFP filter as filter state. Requiring instead that every
`Hostname` Sandbox in a cluster share one address would have kept the static
`STRICT_DNS` cluster; that was rejected, because a per-instance endpoint is exactly the
shape the managed backends this is aimed at expose. The control plane is unaffected,
since a Go client dials whatever it is told.

The path prefix is the only shape touching the protocols, and both cases work out:
`NewProcessClient` appends the procedure to the base URL, so a prefixed base URL
yields `/kruise/<id>/<port>/process.Process/Start` and the front's rewrite restores
it; and `Map` splits with `SplitN(path, "/", 3)`, so a `/files` request keeps its
query string.

### Route projection and readiness

`sandboxroute.Route` gains the endpoint, because both data planes pick a cluster
per request from the Sandbox's attribute and that decision has to survive the
projection into the gateway registry and the ext-proc store. Resource-version
ordering, identity replacement and deletion watermarks are unaffected.

The Pod-IP readiness test becomes addressability. The leaf resolver owns the
value-level semantics; the shared API-aware projection is
`pkg/utils.IsSandboxAddressable`, because `pkg/cache` is already below
`pkg/utils/runtime` and cannot import runtime back:

```
addressable(sandbox) =
    Mode == Direct   && podInfo.podIP != ""
 || Mode == Hostname && endpoint.address != ""
```

`Direct` keeps today's behavior, and all five sites above call the shared predicate.
This is the change most likely to be missed in review: each of those sites reads as a
harmless nil check.

### Where the resolver lives

A new leaf package (`pkg/sandboxendpoint`) with no in-repository imports, taking
mode, host and port as plain values. Two placements do not work: `pkg/sandboxroute`
imports `pkg/identity`, whose closure already contains `pkg/utils/runtime`, so a
resolver there would close a cycle; and `pkg/servers/e2b/adapters`, where the
routing-key constants live today, would pull `pkg/servers/**` into
`agent-sandbox-controller`'s closure. The constants move down into the leaf package,
with `adapters` referencing them from there. Both constraints are checkable in CI:
`go list -deps ./cmd/agent-sandbox-controller/...` must not gain
`pkg/servers/e2b/adapters`, and the build must stay cycle-free.

### Per-site changes

**A / A' / B.** `resolveBaseURL` / `dialIPFor` / `resolveTransport`
(`client.go:283-325`) ask the resolver instead of reading `Status.PodInfo.PodIP`.
Under `Hostname` the client is an ordinary `http.Client` with no pinned transport, so
the runtime TLS bundle does not apply to the call — see
[TLS, trust and failures](#tls-trust-and-failures).
`proxyutils.DefaultRequestFunc` moves onto the same resolver, dropping the CDP path's
own URL construction.

**C.** ext-proc resolves the route's endpoint per request. `Direct` keeps
`x-envoy-original-dst-host` and gains only an explicit `direct` route marker. Under
`Hostname` it sets the authority, prepends the path prefix, adds the endpoint's headers
and hands the front's host and port to the DFP filter through internal headers — all
with `OVERWRITE_IF_EXISTS_OR_ADD`, applied after the client's own
`request-header-modifier` values so a client cannot substitute them — and clears the
route cache so the selected cluster takes effect.

**D.** Same on the filter side, plus a fail-closed opening move: the filter deletes the
internal endpoint headers and sets the `direct` marker *before* parsing, so every early
`Continue` path stays on today's behavior. The two `ORIGINAL_DST` clusters remain for
`Direct`, including the second one that re-encrypts to the runtime with mTLS; under
`Hostname` that leg belongs to the front, so the `upstream-mtls` branch is skipped and
the front clusters carry the request instead. A mixed cluster therefore runs four
clusters, and the gateway needs runtime client certificates for `Direct` Sandboxes but
not for `Hostname` ones.

**E, `ReturnPodIP`.** A client asks for the IP because it intends to connect, which a
placeholder cannot satisfy, so the extension now returns the resolved address:
`infra.Sandbox.GetIP()` became `GetEndpointAddress()`, which yields the Pod IP under
`Direct`, the front's `host:port` under `Hostname`, and empty when the Sandbox is not
addressable at all. The metadata key keeps its meaning while its value stops being an
IP; the alternatives were rejecting the request or declaring the extension
`Direct`-only.

**F, `status.sandboxIp`.** Unchanged and accurate for `Direct`; under `Hostname` it
may be a placeholder nobody reads.

### TLS, trust and failures

An L7 front must read the request to route, so it terminates the connection and
re-encrypts. Under `Hostname` the caller does not apply the runtime TLS bundle at all —
the front owns that leg, which `sandbox-gateway` can already do
(`enable-runtime-mtls`). This reduces cryptographic directness rather than improving
it: `Direct` with a TLS-capable Sandbox is end-to-end mTLS, while `Hostname` adds a
termination point that sees plaintext and `X-Access-Token`. The justification is the
network constraint, not security.

How the front secures its own hop to the runtime is the front's configuration, like
every other front behavior this proposal leaves to the deployment. Two consequences on
*this* side of the hop are worth stating, because they are this repository's behavior.

`TransportOptionsFor` (`transport.go:266`) refuses to fall back to plaintext for a
Sandbox that advertises `AnnotationRuntimeTLSPort` when the caller has no bundle, and
`Hostname` resolves the front before that switch (`client.go:283`), so such a Sandbox is
called with no client certificate whether or not a bundle is configured. That follows
from the front owning the leg, but it is the one downgrade the code otherwise rejects,
so the per-attempt log values report `transport=front` and `runtimeTLSApplied=false`
instead of TLS-mode values naming a dial that never happened.

The request also rides the ordinary plaintext client, so with `scheme: https` the front's
certificate is verified against the process's system trust store and no client
certificate is offered. Until `AuthSecretRef` lands, `Hostname` therefore needs a front
that requires no credential of its own and whose certificate is publicly trusted; with
`scheme: http` the access token crosses that hop in cleartext, which is how the ACK
validation ran.

Which port the caller renders is ours too — `tlsPort` when TLS is enabled, `RuntimePort`
otherwise — but under `Hostname` it no longer selects the caller's transport, only the
port the front is asked to reach.

`TransportOptionsFor` keeps every one of today's outcomes for `Direct`.

Control-plane calls already carry the per-Sandbox `X-Access-Token`, the same value
the gateway keeps on the route, so they pass a gateway with `enable-auth` and JWT
verification off (`filter.go:168-177`). Routes with traffic JWT enforcement are the
exception: the gateway demands a JWT bound to the Sandbox ID and UID
(`filter.go:157-163`), which the manager does not attach. Preferred fix is a separate
control-plane ingress on the front enforcing `X-Access-Token` and not traffic JWT;
until it exists, `Hostname` is unsupported for Sandboxes that set
`RequireTrafficAuth`.

The runtime client treats 4xx as permanent and 5xx as transient
(`APIError.IsClientError`), so a front must return 5xx — not 404 — for a Sandbox it
cannot route to yet, or startup retries stop. Two local failure classes are separated
the same way: a transport misconfiguration, such as an unusable TLS bundle, is
permanent and reported on the first attempt, while an endpoint that is not addressable
yet stays retryable, because that is the normal state of a Sandbox whose writer has
not published an address.

Layering: the resolver holds no API models or business rules, and takes the
endpoint's fields plus the resolved credential as plain values; no package-global
switch and no feature gate, since `pkg/features` is controller-only.

## Configuration

No new flags on any component, and nothing to configure per Sandbox: the writer derives
the endpoint and every consumer reads it off the object. Only the Envoy configs change,
gaining a DNS cache, a `dynamic_forward_proxy` filter and the `sandbox_front_http` /
`sandbox_front_https` clusters, with route branches selected by the internal route
header — `config/sandbox-manager/envoy-config.yaml` for the manager,
`config/sandbox-gateway/configmap.yaml` plus the runtime-mTLS overlay patch for the
gateway. `Direct` traffic keeps its `ORIGINAL_DST` clusters, so a deployment that never
writes an endpoint behaves exactly as today. The spike also pins the manager's Envoy
image to `v1.37.3` (from `v1.33-latest`); a deployment must confirm its own Envoy
version supports the DFP and `set_filter_state` configuration used.

`endpoint.address` is not `--e2b-domain`, which is the client-facing domain in API
responses.

## L7 front requirements

- Routes to the Sandbox **and port** on whichever slots the templates use. A front
  that cannot derive the upstream port from a request needs one rule per
  `(Sandbox, port)` pair.
- Routes on `Host` / `:authority` without requiring it to match the TLS SNI.
- Strips the path prefix and restores the original path, for the path slot.
- Supports arbitrary header-value routing if the header slot is selected. This is
  not a native general-purpose capability of ingress-nginx.
- Preserves the protocol required on each hop. The caller-to-front and
  front-to-runtime hops are separate: the public envd tested on plaintext 49983
  supports Connect over HTTP/1.1, not h2c gRPC; a gRPC deployment needs TLS/ALPN or
  a runtime build that explicitly supports h2c.
- No body buffering, for `/files` uploads and streaming RPCs.
- Idle timeout above the longest expected stream — upstream E2B uses 610s to stay
  above the 600s GCP LB limit.
- Returns 5xx, not 404, for a Sandbox it cannot currently route to. ingress-nginx
  returns 404 by default, so deployments must configure this behavior explicitly.

## Compatibility and Rollback

- Nil `status.endpoint` is `Direct`: no migration, and no existing field changes
  meaning.
- Additive optional status field, so `make generate manifests` plus a CRD update
  before the controller starts writing it. Rollout order: CRD, consumers, then the
  controller.
- A process that ignores the field treats every Sandbox as `Direct` — correct for
  `Direct`, visibly broken for `Hostname`.
- Until something writes the field, behavior is unchanged everywhere: it stays nil and
  every Sandbox resolves `Direct`.
- Rollback is asymmetric: the attribute is on the object, so reverting a process does
  not revert addressing. Clearing `status.endpoint` is the remedy, and it takes an
  explicit null patch — nothing else clears a value a writer put there.

## Risks

- **An unaware consumer dials a placeholder.** The shared predicate and resolver are
  the only sanctioned way to turn a Sandbox into a target, and the rollout order puts
  consumers first. Main risk of moving the decision onto the object.
- **Client-controllable routing keys.** In header mode the key is an ordinary request
  header, so a front routing purely on it must not face untrusted clients unless it
  validates tokens itself. The CDP port has no token at all. Both data planes therefore
  treat the internal endpoint headers as control-plane output only: the gateway filter
  deletes them before parsing and ext-proc overwrites them after the client's own
  header modifiers.
- **A field nothing here writes yet.** A writer that breaks the contract above sits
  outside these tests, so what consumers enforce locally is all that protects them:
  `Validate` on every resolution, unknown modes failing closed, and `Hostname` with no
  address resolving to "not addressable" rather than to a Pod IP.
- **A stale endpoint is followed silently.** Merge-patch omission and phase jumps can
  leave a previous generation's endpoint in place, and unlike a stale Pod IP it will
  not simply fail to connect — it may reach a live front and the wrong Sandbox. The
  write rules above are the mitigation; the runtime's per-Sandbox `X-Access-Token`
  is the backstop.
- **Secret read scope.** Per-Sandbox credentials give three components read access
  to Secrets they did not need before, and a tenant-influenced reference would be a
  confused deputy — hence the fixed namespace. Dropping the intent API removes that
  class rather than mitigating it: nothing outside the controller contributes to the
  field. Credentials must not reach logs, as the existing route rule requires.
- **Protocol support differs by hop.** ACK validation confirmed that
  `connect.WithGRPC()` fails even when directly connected to the public envd on
  plaintext 49983 because that listener does not support h2c. Separately,
  `newPinnedTransport` does not set `ForceAttemptHTTP2`, and Go does not auto-enable
  HTTP/2 with a custom `DialContext` plus a non-nil `TLSClientConfig`. A deployment
  must validate the exact client-to-front and front-to-runtime protocol pair rather
  than assuming end-to-end HTTP/2.

## Test Plan

Table tests for the resolver (both modes, each slot alone and combined, every
validation rule) and for `addressable` (notably a `Hostname` Sandbox with a
placeholder Pod IP, addressable on its host alone). Each slot round-trips through
`adapters.E2BAdapter.Map`. An anti-drift test asserts the control plane, ext-proc and
gateway filter emit identical authority, path and headers for one input. A dependency
assertion keeps `pkg/servers/e2b/adapters` out of the controller's closure. One e2e
job runs a **mixed** SandboxSet — a single-mode e2e would pass even if the
per-Sandbox decision were ignored.

Since nothing here writes the field, unit tests and that e2e set `status.endpoint`
through the status subresource. They therefore cover the consumers and the shared
readiness rule; the writer contract is verified wherever a writer eventually lives.

### Validated spike evidence

The partial implementation is published at
[`BSWANG/agents:spike/endpoint-addressing-ack-validated`](https://github.com/BSWANG/agents/tree/spike/endpoint-addressing-ack-validated).
In an ACK deployment, one manager process resolved a mixed SandboxSet per object:

```text
Direct:   http://172.26.98.95:49983/json/version
Hostname: http://10.16.2.10:80/json/version
```

ingress-nginx selected the authority rule for the Hostname Sandbox and forwarded to
`172.26.98.96:49983`. With that Sandbox's reported Pod IP replaced by the placeholder
`169.254.1.1`, the projected route remained `running` and the request still connected
to the front. The final `/json/version` response was 404 because the envd listener on
49983 does not serve the CDP path; this validates endpoint selection and routing, not
functional CDP support on that port.

During that run the endpoint was published by an annotation-driven writer, which this
revision removes: the branch no longer carries an intent annotation, and the same
scenario is reproduced by patching `status.endpoint` on the status subresource. Nothing
about the resolved paths above depends on where the field came from.

## Alternatives

**A process-level profile with a derived endpoint** — one mode plus templates per
process, endpoint computed from `(sandboxID, port)`. Cheaper, with no API change and
rollback by restart, but it cannot express a mixed fleet, and cannot tell a
placeholder Pod IP from a real address.

**An intent annotation or spec field** — a template on the SandboxSet propagated to
each Sandbox, rendered by the controller. It is how the spike was first validated, and
it was dropped: the deciding input is the backend, which the controller already knows,
so the knob adds an API surface, a template language and a placeholder set to express
something nobody chooses per Sandbox — while showing up on every end user's object.

**Routing headers on the object** — the header value is the Sandbox ID the object
already carries, and the names belong to the front.

**The endpoint under `status.podInfo`** — not a Pod property, and `podInfo` may be
empty.

**Extend `AnnotationRuntimeURL`** — ignored in TLS mode, misses the CDP and
data-plane paths, cannot express a port, and has no writer today.

Prior art that does not fit: E2B's per-node orchestrator (a DaemonSet plus a
Sandbox-to-node catalog here, and still broken when the Pod network is unreachable
from outside), `kubernetes-sigs/agent-sandbox`'s router resolving a headless Service
DNS name (same client-side shape, still needs Pod reachability), Coder's reverse
tunnel (weakest network requirements, but outbound connection management in
`agent-runtime`), and `pods/proxy` (no new component, weak HTTP/2 streaming).

## Implementation Plan and Status

1. **Implemented:** API `status.endpoint`, mode type, generated deepcopy and CRD
   manifests.
2. **Implemented:** leaf `pkg/sandboxendpoint` resolution and validation — mode,
   address, scheme, path prefix, authority, header names, unknown placeholders and
   `{port}` presence — plus value-level addressability and the API-aware
   `pkg/utils.IsSandboxAddressable` projection. Consumers reject unknown modes rather
   than treating them as `Direct`. Moving the routing-header constants down into the
   leaf package remains.
3. **Implemented:** `sandboxroute.Route` projects a deep-copied endpoint and resolves it
   per request; the Pod-IP gate and the other four readiness sites use the shared
   addressability rule.
4. **Implemented:** `pkg/utils/runtime` and `pkg/utils/proxyutils` resolve per Sandbox.
   Runtime and CDP paths were exercised in a real mixed ACK deployment.
5. **Partially implemented:** the endpoint accessor `GetEndpointAddress()` and
   `ReturnPodIP` on top of it. Secret resolution through informer-backed caches, RBAC
   and `AuthSecretRef` are not implemented, so until they are, `Hostname` presupposes a
   front that needs no credential of its own and, under `scheme: https`, a publicly
   trusted certificate.
6. **Deliberately out of scope:** the writer, and any intent API. The contract under
   [Design](#the-attribute) is normative for whatever eventually sets the field; tests
   set `status.endpoint` directly.
7. **Implemented:** manager ext-proc and gateway filter decoration, DFP cluster
   selection and the Envoy configuration for both data planes. Unit tests cover them;
   neither has been exercised under real traffic.
8. **Partially implemented:** unit tests plus the mixed-fleet ACK validation of the
   control plane. Cross-component anti-drift, adapter round trips for every slot,
   deployment documentation and a maintained e2e job remain.

All of the above is on the validated spike branch. What it proves is the control-plane
resolver, the shared readiness contract and the shape of both data planes — not the
credential design, and not either data plane under real traffic.

## Decided

- **Intent is not an API.** No annotation, no spec field: the endpoint is derived from
  the backend by whatever writes it, and the same field, resolver and consumers serve
  every deployment (2026-08-20).
- **The writer is out of scope for this iteration.** The field stays nil until a
  controller extension or a deployment's own webhook sets it.
- **Per-Sandbox addresses are supported.** Both data planes carry
  `dynamic_forward_proxy` clusters instead of requiring one shared front address.
- **The manager-side Envoy data plane is in scope**, and is implemented alongside the
  gateway rather than deferred.

## Open Questions

1. **Which namespace holds the auth Secret** — the Sandbox's own, which spreads
   Secret read access across all sandbox namespaces, or the system one, which keeps
   RBAC narrow but makes the credential an operator artifact rather than a tenant
   one?
2. **Front authentication**: a separate control-plane ingress, or should
   control-plane calls present a traffic JWT like any other client?
3. **Which port to render for runtime calls, and what listens there.** ACK
   validation confirmed that `utils.RuntimePort` (49983) is served by envd — the
   published `agent-runtime` image builds upstream envd and copies it into the
   sandbox container — and that its plaintext listener speaks Connect over
   HTTP/1.1 but not h2c gRPC. `RuntimeTLSPort` (49984) is a separate listener the
   controller advertises through `AnnotationRuntimeTLSPort`, and this repository
   does not contain whatever serves it, so a port may select not merely an address
   but a different server with a different protocol. Meanwhile the gateway applies
   its runtime-mTLS branch when `sandboxPort == utils.RuntimePort`, i.e. toward the
   plaintext envd listener; the gateway owner should confirm that is intentional.
   The endpoint mechanism can express every port, but which port serves which API
   is a deployment prerequisite rather than an addressing detail. The two planes also
   name different ports as "the runtime": the client renders 49984 whenever TLS is
   enabled, while the gateway keys its mTLS branch on 49983.
4. **Placeholder Pod IPs**: should a writer keep publishing one under `Hostname`, or
   leave `sandboxIp` and `podInfo.podIP` empty? Consumers no longer care, but
   `kubectl` output and operators do.

## Implementation History

- [x] 2026-08-13: Proposal drafted.
- [x] 2026-08-17: Reframed around a per-Sandbox endpoint attribute after the
      mixed-fleet and placeholder-IP requirements.
- [x] 2026-08-18: Published API, resolver, runtime/CDP integration, shared
      addressability and route projection in
      [`spike/endpoint-addressing-ack-validated`](https://github.com/BSWANG/agents/tree/spike/endpoint-addressing-ack-validated).
- [x] 2026-08-18: Deployed the spike to ACK and validated mixed Direct/Hostname
      selection plus placeholder-Pod-IP behavior through the real manager and
      ingress-nginx.
- [x] 2026-08-18: Completed both data planes — ext-proc and gateway decoration, DFP
      cluster selection, Envoy configuration — and the endpoint accessor behind
      `ReturnPodIP`.
- [x] 2026-08-20: Settled the writer question. The field, resolver and consumers are
      the same for every deployment; the endpoint is derived by whatever knows the
      backend's addressing rule; no intent annotation or spec field is added; and the
      writer itself is deferred, so the annotation-driven writer was removed from the
      spike branch.
- [ ] Credentials (`AuthSecretRef`), cross-component anti-drift tests, data-plane
      validation under real traffic, and maintained e2e coverage.
