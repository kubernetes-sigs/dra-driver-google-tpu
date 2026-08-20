# Proposal: KEP-5007 Device Binding Conditions for the TPU DRA driver

> Status: draft for maintainer discussion (intended to be filed as a GitHub issue
> on kubernetes-sigs/dra-driver-google-tpu before any implementation).
>
> Kubernetes facts below are cited against `kubernetes/kubernetes@c44d2a82ef7`
> (origin/master, post-v1.37.0-rc.0) as `k8s:<path>:<lines>`, and against
> `kubernetes/enhancements@8263d295` as `KEP-5007 §<section>` /
> `KEP-4817 §<section>`. See "Sources" at the end.

## Summary

[KEP-5007 (DRA Device Binding Conditions)](https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5007-device-attach-before-pod-scheduled)
lets a driver declare, per published device, condition types that must become
`True` in the ResourceClaim's per-device status before the scheduler binds a pod
to a node — plus failure condition types that, if `True`, make the scheduler
abort the binding, deallocate the claim and reschedule the pod
(KEP-5007 §Proposal, §PreBind Process). The API (`Device.BindingConditions`,
`Device.BindingFailureConditions`, `Device.BindsToNode`;
`k8s:staging/src/k8s.io/api/resource/v1/types.go:467-514`) is gated by
`DRADeviceBindingConditions` (alpha 1.34, **beta and on by default since 1.36**;
`k8s:pkg/features/kube_features.go:1466-1469`) together with
`DRAResourceClaimDeviceStatus` (**GA and locked on in 1.37**;
`k8s:pkg/features/kube_features.go:1527-1531`). This repo already vendors
`k8s.io/api v0.37.0-rc.0`, which carries the fields.

The TPU driver implements neither side of the feature today: it never sets
`BindingConditions`/`BindingFailureConditions` on published devices, and there
is no component that writes device conditions into ResourceClaim status (the
repo has no controller binary at all — `values.yaml` contains a `controller:`
block with no backing template). This document analyzes whether the feature is
*warranted* for TPUs, and lays out the design we would follow if it is.

**Recommendation (TL;DR): "not now, by design."** In the driver's current
node-local, all-chips-per-node model there is no genuine window in which a
published TPU device is allocatable but not yet ready to bind, so a binding
condition would either be set unconditionally (pure scheduling latency with no
signal) or would duplicate health checks that already gate publication. We
propose recording that decision with explicit re-open triggers (§5), and
sketching the implementation (§4) so it is ready to pick up the moment one of
those triggers lands.

## 1. Background: what binding conditions buy, and when

The KEP's motivating cases are devices with a real gap between *allocation* and
*usability on the chosen node*: fabric-attached accelerators that must be
attached after node selection, FPGAs that need reprogramming, devices with
long asynchronous initialization (KEP-5007 §Summary, §User Stories). The
mechanics, as implemented at c44d2a82ef7:

1. Driver publishes devices with `bindingConditions` (and, for pools that are
   not node-local, optionally `bindsToNode: true` so the scheduler pins the
   claim to the chosen node and an external controller can read the node name
   from the claim; KEP-5007 §Proposal).
2. At allocation the scheduler copies both condition lists into
   `claim.status.allocation.devices.results[*]`
   (`k8s:staging/src/k8s.io/dynamic-resource-allocation/structured/internal/experimental/allocator_experimental.go:416-422`;
   API `types.go:2275-2299`) and, when it reserves the claim for the pod,
   stamps `allocation.allocationTimestamp` if unset
   (`k8s:pkg/scheduler/framework/plugins/dynamicresources/dynamicresources.go:1933-1943`).
   Note the allocator deliberately orders pools that carry binding conditions
   *after* pools that don't
   (`k8s:.../structured/internal/experimental/pools_experimental.go:128-190`),
   so such devices are a lower-preference choice when alternatives exist.
3. In PreBind, if any allocated device has binding conditions, the scheduler
   emits a `BindingConditionsPending` event and polls the claim every 5s until
   every binding condition is `status: True` and no failure condition is
   `True` (only `.status` is evaluated), or until the binding timeout
   (`dynamicresources.go:1705-1731, 1995-2023`). Two things are true while it
   waits: the pod has already been *assumed* onto the node in the scheduler
   cache (assume and Reserve run in the scheduling cycle before the binding
   cycle's PreBind — `k8s:pkg/scheduler/schedule_one.go:328, 339, 466`), so
   its CPU/memory and the claim's devices are held for the whole wait; and
   with `NominatedNodeNameForExpectation` (beta, on by default since 1.35;
   `k8s:pkg/features/kube_features.go:1921-1924`, KEP-5278) the scheduler
   writes `pod.status.nominatedNodeName` before the wait so external
   components such as the cluster autoscaler can see the pending placement
   (`schedule_one.go:412-429`; KEP-5007 §Risks and Mitigations names this as
   the mitigation for autoscaler scale-down). Waiting pods are also visible
   through the ALPHA scheduler metrics
   `scheduler_dra_bindingconditions_wait_duration_seconds` and
   `scheduler_dra_bindingconditions_allocations_total`, labelled by driver and
   outcome (`k8s:pkg/scheduler/metrics/metrics.go:540-560`;
   `dynamicresources.go:1733-1768`).
4. A driver-side component writes `metav1.Condition{Type: <bindingCondition>,
   Status: True}` into `claim.status.devices[*].conditions` — or a failure
   condition on error — via the `resourceclaims/status` subresource
   (KEP-5007 §PreBind Process, §Handling attachment failures).
5. Scheduler proceeds to bind. On a failure condition or timeout PreBind
   returns an error, the pod is retried, and in the next cycle Filter marks the
   claim unavailable and PostFilter deallocates it (clearing
   `status.allocation`, `status.reservedFor` and `status.devices`)
   (`dynamicresources.go:988-1000, 1147-1200`). The timeout is
   `DynamicResourcesArgs.BindingTimeout`, default 600s, measured from
   `allocationTimestamp`
   (`k8s:pkg/scheduler/apis/config/types_pluginargs.go:261-289`,
   `k8s:pkg/scheduler/apis/config/v1/defaults.go:254-258`,
   `dynamicresources.go:2030-2046`). Rescheduling is best-effort sticky, not
   a blank restart: a pod carrying `nominatedNodeName` has that node evaluated
   first in the retry (`schedule_one.go:665-676`), so a transient failure on
   an otherwise-fit node usually lands back on it.

Net: what binding conditions buy is that no kubelet-side work (Prepare, CDI,
container creation) starts until the device is confirmed ready, the wait is
observable and bounded, and failure produces a reschedule instead of a pod
wedged in ContainerCreating. They do **not** free the node's CPU/memory or the
devices during the wait — those are held from assume onward either way.

Two constraints matter for design: the KEP describes the failure/reschedule
path as "an exception, not the primary scheduling model", asks the controller
setting a failure condition to also ensure the device is not picked again
(remove it from the slice, taint it, or change its node selector), and records
that "fail and reschedule" was clarified to be an anti-pattern (KEP-5007
§Handling attachment failures, §Notes/Constraints, §Implementation History
2025-06). And after deallocation the API server automatically drops
`status.devices` entries for the deallocated devices
(`k8s:pkg/registry/resource/resourceclaim/strategy.go:200, 337`), so
condition cleanup is not the driver's job.

The reference implementation in dra-example-driver has both halves:

- **Publishing** — `internal/profiles/gpu/gpu.go:183-188` stamps
  `BindingConditions`/`BindingFailureConditions` on every published device,
  gated by a `--binding-conditions` flag (Helm value
  `kubeletPlugin.bindingConditions`, default `false`).
- **Condition writer** — `cmd/dra-example-controller/`, a controller-runtime
  manager watching ResourceClaims, filtering to claims with allocation results
  for this driver, with a `BindingConditionsPlugin`
  (`plugins/bindingconditions.go`) that sets the condition to
  `True/"Ready"`. Helm deploys the controller Deployment (single replica, no
  leader election) only when `controller.plugins` is non-empty.
- **e2e** — `test/e2e/e2e_test.go` "BindingConditions" context: verifies
  publication in ResourceSlices and that a pod still reaches Running.

Notably, even the example's condition writer is a stub: it marks devices ready
unconditionally, because the simulated GPUs have no real not-ready state. That
is exactly the situation we want to avoid replicating for TPUs. The drivers
the KEP cites as beta feedback both had a real asynchronous step to model:
CoHDI's fabric attach, and NVIDIA's ComputeDomain (IMEX daemons scheduled and
healthy across nodes) (KEP-5007 §Graduation Criteria / Beta).

## 2. Design question 1: is there a real not-ready window for TPUs?

We examined the three candidate readiness signals from the driver's actual
lifecycle. All three turn out to be resolved *before* the driver publishes a
ResourceSlice, which means "published" already implies "bindable" today:

**(a) TPU runtime / device files present.** At startup `NewDriver`
(`driver.go:73-80`) selects the device directory by TPU generation — `/dev`
for v3/v4/v4lite, `/dev/vfio` for v5p/v5lite/v5litepod/v6e — and device
discovery (`manager.go:enumerateAllPossibleTpuDevices`) walks it and
*hard-fails driver startup* if the enumerated
device count doesn't match the node's chip-count label
(`"enumerated tpu devices not equal to chipCount"`). No slice is published
until every chip's device file exists. After startup, `TPUHealthChecker`
re-stats the device files every 10s and a device that disappears is dropped
from the published slice entirely (`driver.go:GatherStateAndPublish` only
publishes `device.allocatable == true`), making it unallocatable — a stronger
remedy than a binding condition, applied at the same detection latency.

**(b) Multi-host slice topology assembled.** `TPU_TOPOLOGY`, `HOST_BOUNDS`,
`TPU_HOST_BOUNDS`, etc. (`util.go:InitEnvs`/`addPodsliceOrSliceEnvs`) are
computed once at startup from static inputs — GKE node labels or GCE metadata
(`kube-labels`/`tpu-env`). The driver does not orchestrate cross-host slice
formation; ICI fabric assembly happens in GCE provisioning before the node
joins the cluster and is not observable as an asynchronous "assembling →
ready" transition from this driver. There is nothing for a condition to wait
on. (Multi-host *workload* coordination — JobSet, TPU_WORKER_HOSTNAMES — is a
workload-level concern above DRA binding.)

**(c) vfio binding for v5+/v6e.** Also completed by boot-time node setup. If it
hasn't happened, `/dev/vfio` enumeration fails and the driver refuses to start
(case (a) again) — the plugin DaemonSet crash-loops and no slice exists.

**The one residual window** is allocation-to-binding races: a chip's device
file vanishes *after* the scheduler allocates the claim but *before* the pod
binds. Binding conditions could catch this (the condition writer would check
health and set a failure condition, causing deallocation instead of a pod stuck
in ContainerCreating with failing `Prepare` calls). This window is real but is
(i) seconds wide, (ii) shared by every node-local driver — the sibling
kubernetes-sigs NVIDIA GPU driver has not adopted binding conditions for plain
GPU/MIG devices either (its counterpart proposal identifies only ComputeDomain
IMEX channels, a genuinely asynchronous cross-node readiness step, as a use
case), and (iii) on TPU nodes a vanished chip under the all-chips-allocation
model means the whole node is effectively broken — node-level repair, not
claim-level rescheduling, is the remedy. We don't think it justifies a new
cluster-scoped controller.

**Conclusion:** with today's architecture the condition writer would have
nothing real to verify — it would be the example driver's unconditional stub
with a TPU label on it, adding a controller deployment, `resourceclaims/status`
write RBAC plus the per-driver `resourceclaims/driver` authorization described
in §3, a 5s-granularity PreBind round trip on every TPU pod during which the
node's resources and all its chips are already held (§1 step 3), and a lower
allocator preference for TPU devices (§1 step 2), for no added safety. The
observability and best-effort-sticky rescheduling that come with the wait
(§1 steps 3 and 5) are real, but they only pay off when there is something to
wait for.

## 3. Design question 2: who would write the condition?

Recorded for when the feature is warranted (§5); this also informed §2's cost
assessment.

**Authorization model (changed since the first draft of this proposal).**
Since 1.36 the beta, on-by-default gate `DRAResourceClaimGranularStatusAuthorization`
(KEP-4817; `k8s:pkg/features/kube_features.go:1533-1535`) makes the API
server perform a per-driver authorization check on every `status.devices`
write, in addition to ordinary `resourceclaims/status` RBAC: the writer needs
verb `arbitrary-node:update` (any identity) or `associated-node:update` (a
service account whose bound token carries the node-name claim of the node the
claim is allocated to) on the synthetic subresource `resourceclaims/driver`,
with `resourceNames` scoped to the driver name being written
(`k8s:pkg/registry/resource/resourceclaim/strategy.go:215-224`,
`k8s:pkg/registry/resource/utils.go:150-200, 277-300`,
`k8s:staging/src/k8s.io/api/resource/v1/types.go:53-66`). The example driver's
chart already carries the `associated-node:*` rule for its kubelet plugin
(`deployments/helm/dra-example-driver/templates/clusterrole.yaml`).

Two viable writers follow from that:

- **A central controller (example-driver pattern) — still recommended.**
  Matches the KEP's "external controller" model and the reference
  implementation. RBAC: `resourceclaims` get/list/watch,
  `resourceclaims/status` update/patch, and `resourceclaims/driver`
  `arbitrary-node:update`/`arbitrary-node:patch` with
  `resourceNames: ["tpu.google.com"]`. Cost: a new binary, image target,
  Deployment template (the dead `controller:` block in `values.yaml` finally
  gets a backing template, or is deleted under the §5 "won't do" outcome), and
  a controller-runtime dependency; leader election only if HA is ever wanted
  (the example runs a single replica without it).
- **The kubelet plugin DaemonSet itself — now a legitimate option.** Binding
  conditions must flip while the pod is still unbound, i.e. *before*
  `PrepareResourceClaims` ever fires (all DRA kubelet hooks run post-bind), so
  the plugin cannot reuse its Prepare path — but it could run a claim watch and
  write conditions for claims whose allocation is pinned to its own node. The
  first draft rejected this on the grounds that it meant "cluster-wide status
  write from every TPU node"; with granular authorization that is no longer
  true — `associated-node:update` on `resourceclaims/driver` confines each
  node's identity to claims allocated to that node, and this is precisely the
  case KEP-4817 designed the verb for (KEP-4817 §Write Permission). It still
  requires widening the DaemonSet ClusterRole beyond today's `resourceclaims
  get` (`templates/clusterrole.yaml`) and depends on the cluster's SA tokens
  carrying node claims. We keep the controller as the recommendation for
  parity with upstream, but the security argument against a node-side writer
  is weaker than originally stated and this should be an explicit maintainer
  choice.

The cost of either path is a further argument for not building speculatively.

## 4. Sketch: what we would build when a trigger fires

Mirrors the example driver, adjusted for TPU specifics and for the constraints
verified in §1:

1. **Publishing** — in `AllocatableDevice.GetDevice()` (`manager.go`), gated by
   a `--binding-conditions` flag / `kubeletPlugin.bindingConditions` Helm value
   (default `false`):
   - `BindingConditions: ["tpu.google.com/DeviceReady"]`
   - `BindingFailureConditions: ["tpu.google.com/BindingFailed"]`
   - Condition types are validated as qualified names (label-name syntax) and
     the two lists must not overlap or contain duplicates; at most 4 entries
     each (`k8s:pkg/apis/resource/validation/validation.go:1983-2000`;
     `types.go:351-352`). Domain-qualified types rather than the example's
     bare `"BindingConditions"`, since they land in a shared per-device
     conditions list (max 8 entries, `listType=map` keyed by `type`;
     `types.go:2534-2536, 2590-2602`) that other components can also write to.
   - `BindsToNode` stays unset: for a slice published with `spec.nodeName`
     the allocator already sets `claim.status.allocation.nodeSelector` to
     `metadata.name In [<node>]` — the same selector `BindsToNode` would
     produce for a non-node-local pool
     (`k8s:.../structured/internal/experimental/allocator_experimental.go:2125-2147`).
   - The vendored `resourceslice` helper detects when the API server strips
     the fields because the cluster gate is off and reports a
     `DroppedFieldsError` through the plugin's `HandleError` hook
     (`vendor/k8s.io/dynamic-resource-allocation/resourceslice/resourceslicecontroller.go:293-326`);
     the driver must log that loudly rather than silently run without
     conditions.
2. **Controller** — `cmd/tpu-dra-controller/`, controller-runtime manager
   watching ResourceClaims, filtered to claims with allocation results where
   `result.Driver == "tpu.google.com"`; plugin-style layout copied from the
   example so future claim-status features (e.g. device-status enrichment)
   slot in. Deployed by Helm only when `controller.plugins` is non-empty, with
   the RBAC from §3. Condition writes must be idempotent and finish well
   inside the 600s scheduler timeout.
3. **Readiness check with real content** — beyond "mark ready":
   - Re-check device health via the published slice before setting
     `DeviceReady` (closes the §2 residual race); on a vanished device set
     `BindingFailed` *and* let the existing health path drop the device from
     the slice so it is not re-picked, as the KEP requires.
   - **Partial-claim rejection is *not* a fit for failure conditions.** Today
     a claim requesting fewer than all chips is allocatable and only fails in
     `Prepare` (`state.go:197-199`), after binding, wedging the pod in
     ContainerCreating (this is the interaction with the all-chips constraint —
     design question 3 of the parity review). The first draft proposed setting
     `BindingFailed` for such claims; on inspection that would loop —
     PostFilter deallocates, the allocator re-allocates the same partial set,
     the controller fails it again every cycle — which is exactly the "fail
     and reschedule" anti-pattern the KEP warns against, and there is no
     device the controller could legitimately remove from the slice to stop
     the loop. The right fix is admission-time validation of the claim (a new
     ValidatingAdmissionPolicy — the repo's existing one only guards
     ResourceSlice writes) or a scheduler-visible capacity model; that is a
     separate issue.
4. **Cluster requirements** — document that consumers need
   `DRADeviceBindingConditions` enabled (beta, on by default since 1.36;
   `DRAResourceClaimDeviceStatus` is GA and locked on in 1.37) and, on 1.36+,
   the `resourceclaims/driver` RBAC from §3; keep the driver default off so the
   chart stays deployable on 1.35-and-earlier clusters or ones where the beta
   gate was explicitly disabled. Note that KEP-5007's `kep.yaml` lists
   `stable: v1.37` as the target but the v1.37.0-rc.0 code still registers the
   gate as beta, so plan for beta semantics.
5. **Tests** — unit tests for the publishing gate and the controller's
   condition logic (table-driven, fake client, mirroring `controller_test.go`
   in the example); e2e case patterned on the example's BindingConditions
   context once this repo grows an e2e harness.

## 5. Decision asked of maintainers

We propose **Option A** and will implement Option B instead if maintainers
prefer parity now:

- **Option A — record "not now" (recommended).** Close with this analysis;
  delete or annotate the dead `controller:` block in `values.yaml`. Re-open
  when any of these becomes true:
  - the driver gains an asynchronous per-claim provisioning step (dynamic vfio
    (re)binding, TPU reset/repair between workloads, runtime slice
    reconfiguration);
  - slice-level scheduling arrives, where multi-host slice assembly for a
    claim is asynchronous and observable;
  - ICI-resiliency repair flows want to hold binding while a slice heals.
- **Option B — flag-gated parity implementation.** Build §4 now, default off,
  primarily as feature-parity/conformance with the example driver, accepting
  that the ready-condition is initially unconditional; choose controller vs
  node-side writer per §3.

The kubernetes-sigs NVIDIA GPU counterpart effort reached the same "no genuine
not-ready window" reading for node-local GPU/MIG devices and is proposing
binding conditions only for its ComputeDomain (IMEX channel) devices, which do
have an asynchronous readiness step; we intend to keep the two decisions
consistent and cross-link the issues.

## Sources

Kubernetes (`kubernetes/kubernetes@c44d2a82ef7`, origin/master, after
v1.37.0-rc.0):

- Feature gates — `pkg/features/kube_features.go:1466-1469`
  (`DRADeviceBindingConditions`: alpha 1.34, beta/default-on 1.36),
  `:1527-1531` (`DRAResourceClaimDeviceStatus`: alpha 1.32, beta 1.33,
  GA/locked 1.37), `:1533-1535` (`DRAResourceClaimGranularStatusAuthorization`:
  beta/default-on 1.36), `:2589` (dependency of BindingConditions on
  DeviceStatus).
- API — `staging/src/k8s.io/api/resource/v1/types.go:53-66` (synthetic
  subresources/verbs), `:351-352` (max 4 conditions), `:467-514`
  (`Device.BindsToNode/BindingConditions/BindingFailureConditions`),
  `:2160-2175` (`AllocationResult.NodeSelector`, `AllocationTimestamp`),
  `:2275-2299` (copies in `DeviceRequestAllocationResult`), `:2534-2536,
  2590-2602` (`AllocatedDeviceStatus.Conditions`, max 8).
- Validation — `pkg/apis/resource/validation/validation.go:1819-1822,
  1983-2000`.
- Scheduler — `pkg/scheduler/framework/plugins/dynamicresources/dynamicresources.go:988-1000`
  (Filter marks failed/timed-out claims unavailable), `:1147-1200`
  (PostFilter deallocation), `:1661, 1705-1731` (PreBind wait, 5s poll,
  event), `:1933-1943` (AllocationTimestamp), `:1995-2046`
  (`isClaimReadyForBinding`, `isClaimTimeout`);
  `pkg/scheduler/apis/config/types_pluginargs.go:261-289` and
  `pkg/scheduler/apis/config/v1/defaults.go:254-258` (`BindingTimeout`,
  600s default); `pkg/scheduler/schedule_one.go:328, 339` (assume/Reserve in
  the scheduling cycle), `:412-429` (nominatedNodeName written before
  Permit/PreBind), `:466` (PreBind in the binding cycle), `:665-676`
  (nominated node evaluated first on retry);
  `pkg/features/kube_features.go:1921-1924`
  (`NominatedNodeNameForExpectation`: alpha 1.34, beta/default-on 1.35);
  `pkg/scheduler/metrics/metrics.go:540-560`
  (`dra_bindingconditions_allocations_total`,
  `dra_bindingconditions_wait_duration_seconds`, ALPHA).
- Allocator — `staging/src/k8s.io/dynamic-resource-allocation/structured/internal/experimental/allocator_experimental.go:416-422`
  (copy conditions), `:2125-2147` (node selector for node-local /
  BindsToNode); `pools_experimental.go:128-190` (pools with binding
  conditions ordered last). The `incubating` allocator mirrors both.
- Authorization — `pkg/registry/resource/resourceclaim/strategy.go:200,
  215-224, 337` and `pkg/registry/resource/utils.go:119-200, 277-300`.

KEPs (`kubernetes/enhancements@8263d295`):

- KEP-5007 `kep.yaml` (stage beta, latest-milestone v1.36, milestones alpha
  v1.34 / beta v1.36 / stable v1.37 target); README §Summary, §Proposal,
  §PreBind Process, §Handling attachment failures, §Notes/Constraints,
  §Risks and Mitigations (cluster-autoscaler visibility, node nomination as
  mitigation), §Scheduler DRA Plugin Modifications, §PreBind Phase Timeout,
  §Graduation Criteria, §Implementation History.
- KEP-5278 `kep.yaml` (Nominated node name for an expected pod placement:
  stage beta, alpha v1.34 / beta v1.35).
- KEP-4817 `kep.yaml` (stage stable, milestones alpha v1.32 / beta v1.33 /
  stable v1.36 — the code above locks the gate in 1.37); README §Write
  Permission (note its RBAC example predates the `resourceclaims/driver`
  subresource actually implemented).

Reference implementation (kubernetes-sigs/dra-example-driver): publishing
`internal/profiles/gpu/gpu.go:183-188`; controller `cmd/dra-example-controller/`
(`plugins/bindingconditions.go`, `controller.go`); Helm `controller.plugins`
gating, `templates/controller-deployment.yaml` (`replicas: 1`),
`templates/clusterrole.yaml` (`resourceclaims/driver` rule); e2e
`test/e2e/e2e_test.go` "BindingConditions" context.

This repo: discovery/publication `cmd/tpu-dra-kubeletplugin/manager.go`,
`driver.go:73-80, 134-159`; health gating `health_check.go`,
`state.go:UpdateHealth`; all-chips constraint `state.go:197-199`; RBAC
`deployments/helm/dra-driver-google-tpu/templates/clusterrole.yaml`; dead
controller Helm block `values.yaml:30-46`; vendored resourceslice helper
`vendor/k8s.io/dynamic-resource-allocation/resourceslice/resourceslicecontroller.go:293-326`.
