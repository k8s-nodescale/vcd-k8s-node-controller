# `spec.ingressPool` on two platforms - concept for owner review

Status: **proposal, 2026-11-15. No code until the owner agrees on the model.**

> **Owner direction, 2026-11-15 (supersedes the recommendation below where they
> differ):** keep the item on the roadmap, but the vSphere workflow is
> **optional** - a pool that does not define it must not raise an error - and
> when it is defined the controller may talk to HAProxy directly (usually 2+
> HAProxy VMs in the same network as the workers) and update its configuration,
> possibly through an HAProxy API. So section 4's "declare-only now" is no
> longer the plan: the plan is the optional managed workflow described in
> `project_roadmap.md`. Sections 3, 5, 6 and 10 remain useful input for it, and
> section 3 option B (publish membership, an agent applies it) is the fallback
> if HAProxy cannot be driven directly.
Scope: what `spec.ingressPool` means on vCD and on vSphere, what the controller
is allowed to write, and what `status` and the page say when it is not the one
holding the membership. Nothing here changes vCD behaviour.

## 1. Why this needs a decision

On vCD the field has exactly one meaning and the controller is the only writer:
a worker is added to an NSX-V edge server pool when its Node turns Ready, and
removed before the drain starts. The controller can reach the edge gateway with
the same credentials it uses to place VMs, so "managed by nodescale" is true.

On vSphere there is no equivalent object. The reference installation runs a pair
of HAProxy + keepalived VMs (the `lb-01`/`lb-02` addresses the address plan
already reserves through `ipPlan.lbNodeOffset` / `lbNodeCount`); the controller
does not create them, has no credentials for them, and cannot edit their
configuration. Today the vSphere provider simply does not implement
`cloud.PoolMembership`:

- `membership()` returns `(nil, nil)`,
- `ensureIngressPool` returns before emitting anything,
- `removeFromIngressPool` returns `nil`.

So a vSphere pool that sets `ingressPool` looks configured - the spec says
workers join a pool, the README example shows it - and nothing happens, with no
event, no status field and no page indicator. That silence is the defect this
document is about. It is worse than a refusal, because an operator copies a vCD
pool into a vSphere pool and believes traffic is being balanced.

The field also mixes three different questions:

| Question | vCD answer | vSphere answer |
| --- | --- | --- |
| Where does ingress traffic enter? | NSX-V virtual server on the edge | HAProxy VIP held by keepalived |
| Who owns the member list? | nodescale (through the edge API) | Terraform/Ansible today, nobody at runtime |
| How is a member withdrawn? | the controller edits the pool before the drain | HAProxy health check fails, then keepalived/HAProxy stops using it |

One field cannot answer all three the same way on both platforms, but it can
answer them **honestly**, which is what this proposal is about.

## 2. What the controller does today (for the record)

- `spec.ingressPool = {}`: nothing happens on either platform (the pool is
  optional and an absent pool means "not configured").
- vCD, `type: nsxv`: member added on Ready (`IngressPoolMember` event), removed
  before the drain. A failed removal is not fatal for a scale-down ("the pool's
  health monitor takes the address out once the node stops answering",
  `IngressPoolMemberNotRemoved` warning); a rollout treats it as fatal except
  when the pool is not configured at all.
- `type: nsxt`: rejected with `ErrPoolNotConfigured`, so the worker is drained
  and deleted and the member has to be removed by hand.
- vSphere: silent no-op (this document).

Note that the vCD pool member is a **static, enabled** member pointing at the
node port. Correctness there depends on the controller removing it; the edge's
monitor is only a fallback. That asymmetry matters for section 6.

## 3. Options

### A. Declare-only ("external"): the controller never writes membership

`ingressPool` becomes a description, not a command. The controller publishes
what it knows (name, address plan, which workers are Ready) and states that
membership is maintained elsewhere. It emits one pool-level event, not one per
worker, and stops pretending.

- Cost: a status field, one new enum value, docs, UI text. No new privileges,
  no credentials, no SSH, no new failure modes.
- Honesty: complete, provided the status says `managed: false` and names the
  owner.
- Gap: nothing guarantees the pool actually contains the Ready workers. The
  operator (or Ansible) must keep the two in step. HAProxy health checks make
  this survivable for *removal*, not for *addition*: a new worker is only in the
  pool when something adds it.

### B. The controller publishes membership; an agent applies it

The controller writes the desired member list - a ConfigMap or a small CR,
e.g. `IngressPoolMembers{pool, members: [ip:30080...], generation}` - and
something already living next to the LB VMs (an agent, a systemd timer, an
Ansible pull, a DaemonSet on the LB nodes) renders `haproxy.cfg` and reloads.
The controller owns intent and progress, not transport, and needs no SSH.

- Cost: one new object type, a renderer outside the controller, a contract for
  reload (graceful, `-sf`), and idempotency after an LB VM restart.
- Benefit: the member list is real and observable; `status` can say `in-sync`
  against what the agent reports back.
- This is the shape that would let vSphere catch up with vCD without inventing
  credentials. It is also the only option that keeps "removed before the drain"
  true on vSphere.

### C. The controller drives the LB VMs directly (SSH or an HAProxy API)

Rejected for now. It means shipping credentials for a machine the controller
does not own into a Secret, taking responsibility for `haproxy.cfg` syntax,
reload semantics and keepalived state, and no way to observe the result except
by trusting the exit code of a remote command. The blast radius (every request
to the cluster) is the largest of any controller action.

### D. Split the field per platform (`ingressPool.vsphere.*`)

Rejected. It encodes the platform inside the API, and the second platform would
not be the last: the same question comes back for any installation with an
external LB. `type` already names the implementation; it should name the
ownership model too.

## 4. Recommendation

1. **Now (1.3.0, this document's decision):** option **A** with an honest
   `type`, plus the status/UI contract in sections 5 and 7. `ingressPool.type:
   external` means "a pool something else maintains", and the controller
   reports it as such instead of doing nothing silently. `nsxv` keeps working
   exactly as it does. `nsxt` stays rejected.
2. **Later (1.4.0, if the owner wants vSphere to be managed):** option **B**.
   The same field, `type: external`, gains `mode: managed` with the membership
   object and the agent; `managed` stays false until that exists.
3. Option C is not on the roadmap.

The important part is not the enum value but the rule: **the controller never
claims membership it does not hold.**

## 5. Status contract

New status block, additive (nothing existing changes shape):

```yaml
status:
  ingressPool:
    type: nsxv            # nsxv | external
    managed: true         # false when membership is maintained outside nodescale
    name: demo-cluster-ingress-pool
    # what the controller believes, not what it hopes
    members: 3            # Ready workers it has in the pool (nsxv), or could
                          # have in it (external)
    state: InSync         # InSync | Unmanaged | NotConfigured | Error
    owner: "terraform"    # external only: who maintains it, from a new optional
                          # spec.ingressPool.owner
    lastError: ""         # set when state is Error
    updatedAt: "..."
```

- `state: Unmanaged` is the normal vSphere state. It is not a failure and must
  not colour `Ready`: the pool condition stays `Converged` when the workers are
  Ready, because the workers are fine. The pool's own block is where the caveat
  lives.
- `state: Error` covers a pool that is configured and managed but rejects a
  change (the existing `IngressPoolFailed` / `IngressPoolMemberNotRemoved`
  paths) - today that is only visible in events, which expire.
- Events: one `IngressPoolUnmanaged` (Normal) per pool when the state first
  becomes `Unmanaged`, not per worker. `IngressPoolMember` and
  `IngressPoolFailed` keep their meaning where the controller is the writer.
- No new condition type. The README documents one condition (`Ready`) and the
  page keys off it; a second condition would need a UI change before it is
  useful.

## 6. Drain safety when membership is not ours

This is the part that decides whether option A is defensible.

With a managed NSX-V pool the controller withdraws the member, so no new
connection reaches a node that is about to disappear. With an external pool the
controller cannot withdraw anything, so the pool must do it itself:

- **Requirement:** every member is health-checked on the node port
  (`ingressPool.port`), with a check interval and a fall count such that a node
  that is cordoned and drained stops receiving new connections before its pods
  are gone.
- **Requirement:** the drain is not finished faster than that. The controller
  should wait `healthCheckGrace` (new optional field, default e.g. `30s`, and
  never shorter than one check interval) after cordoning before it starts
  evicting pods.
- **What the controller must refuse:** nothing, if the operator wrote
  `type: external` - the contract is theirs to keep. What it must not do is
  *assume* the health check exists, which is exactly what the silent no-op does
  today.
- **Rollout:** today a failed pool removal fails the rollout step. With an
  external pool there is no removal call, so the rollout proceeds on the
  cordon + grace; the status block records `Unmanaged` so the reason is
  visible if a request does reach the dying node.

For the lab this changes nothing operational: `ingress-nginx` on NodePort,
HAProxy in front, health checks on 30080. It only makes the guarantee explicit
and gives the operator the knob.

## 7. What the page says

Three states, one block:

| State | Page |
| --- | --- |
| `NotConfigured` (field empty) | block hidden, as today |
| `InSync` / `nsxv` | `Ingress pool demo-cluster-ingress-pool - NSX-V edge demo-edge-gateway, 3 members, managed by nodescale` |
| `Unmanaged` / `external` | `Ingress pool <name> - external (HAProxy + keepalived), membership maintained by <owner>; nodescale does not edit it. Health check on port 30080 is required for safe drains.` |
| `Error` | same line with the last error and the worker it names |

The unmanaged line must be visible without opening a detail view: it is the one
place where the page can prevent the mistake of assuming balanced traffic.

UI parity is part of A3's gate: every option added here has to be mirrored and
verified in the web UI before the vSphere Terraform/Ansible is updated.

## 8. API changes

Additive, no version bump of the CRD:

- `spec.ingressPool.type`: add `external` to the enum.
- `spec.ingressPool.owner`: optional string, free text, external only.
- `spec.scaleDown.healthCheckGrace`: optional duration, default `30s`,
  effective for `external` only.
- `status.ingressPool`: new block as in section 5.
- `spec.ingressPool.edge` keeps its meaning and is ignored for `external`.

Old pools are unaffected: `nsxv` behaves as before, an empty `type` keeps
meaning nsxv (the provider default), and a vSphere pool that already carries
`ingressPool` with no `type` changes from "silently nothing" to
`Unmanaged` - which is the bug fix.

## 9. Verification

For option A (1.3.0):

- unit: a vSphere pool with `ingressPool` set reports `Unmanaged` once, adds no
  member, and leaves `Ready` alone; an NSX-V pool is unchanged (existing
  tests); a `type: external` pool on vCD does not call the edge at all.
- lab: pool `nscl-test-avntg` with `type: external`, `owner: terraform`,
  `port: 30080` - scale 2 -> 3 shows `Unmanaged` in status, no
  `IngressPoolFailed` event, drain and rollout unchanged; page shows the
  unmanaged line.
- docs: README table row, and the vSphere limitations list loses the line that
  said pool membership is not available.

For option B (later): two HAProxy VMs in the lab, the agent on them, then
`0 -> 1 -> 3 -> 1` with `curl` against the VIP through each step, and a kill of
one HAProxy to prove keepalived still holds the VIP.

## 10. Open questions for the owner

1. Option A + `type: external` now, B later - or wait for B and reject
   `ingressPool` on vSphere until then? (Rejecting is honest too, but it breaks
   pools that already carry the field and gives the operator nothing.)
2. Should `Unmanaged` be allowed to leave `Ready` at `Converged`? The
   recommendation says yes (the workers are healthy); the alternative is a
   separate `Ready=False` reason, which colours the pool red for a condition the
   controller cannot fix.
3. Who is the owner on vSphere in our installation - Terraform, Ansible, or
   "the network team"? The `owner` field is free text; the page should print
   something the reader can act on.
4. Is `healthCheckGrace` wanted as a field, or should the drain simply trust the
   operator and document the requirement?
5. For B: is a ConfigMap acceptable as the membership object (no CRD, easy to
   read, no admission), or must it be a CR for the page and RBAC?
6. Does the LB pair eventually become a controller-created VM kind (a third
   provider object), or does it stay Terraform's forever? A4 assumes the latter.

## 11. Decision record

- 2026-11-15: document created, awaiting the owner. No code written.
