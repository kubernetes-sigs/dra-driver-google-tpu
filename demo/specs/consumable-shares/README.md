# Sharing TPU chips between claims

By default the TPU DRA driver allocates the chips on a node to exactly one
`ResourceClaim`. With the `ConsumableShares` feature gate and the
`--consumable-shares` flag, several claims can be allocated the same chips.

## Read this first

**Sharing lets claims co-schedule and mount the same chips. It does not let two
processes drive a chip at the same time.** The TPU runtime takes an exclusive
lock on a chip when a process opens it, so a second process that initializes the
runtime on an already-open chip will fail.

That makes sharing useful when:

* Only one container in the sharing set actually opens the TPU, and the others
  are sidecars, exporters, profilers, log shippers, or a proxy in front of a
  single owner process.
* The sharing workloads use the chips one after another and coordinate the
  handoff themselves.

It is *not* a way to run two concurrent jobs on one node.

## Prerequisites

1. **Kubernetes with `DRAConsumableCapacity`.**

   Without it the API server silently drops the sharing fields from the
   driver's `ResourceSlice`s. Nothing errors — pods just never co-schedule. If
   sharing appears to do nothing, check this first:

   ```bash
   kubectl get resourceslice -o yaml | grep -A3 allowMultipleAllocations
   ```

2. **A single-host TPU node.** Sharing is supported only where the chips on the
   node make up an entire slice. On a multi-host slice the driver logs a warning
   and advertises devices without sharing.

3. **The driver's `ConsumableShares` gate.** Unlike the cluster-side gate, this
   one fails loudly: setting `--consumable-shares` to a policy without it is a
   startup error.

## Modes

| `--consumable-shares` | Effect |
| --- | --- |
| `disabled` (default) | One claim holds the chips, as before this feature. |
| `unlimited` | Chips are advertised with `allowMultipleAllocations: true` and no capacity. Any number of claims may bind. |
| `N` (positive integer) | As above, plus a `shares` capacity of `N` with request policy `default: 1, min: 1, max: N, step: 1`. At most `N` claims may bind. |

## Trying it out

Install the driver with sharing enabled:

```bash
helm upgrade -i dra-driver-google-tpu deployments/helm/dra-driver-google-tpu \
  --set featureGates.ConsumableShares=true \
  --set consumableShares=2
```

Then apply the matching example:

```bash
kubectl apply -f integer-sharing.yaml     # for --consumable-shares=2
kubectl apply -f unlimited-sharing.yaml   # for --consumable-shares=unlimited
```

Both pods should land on the same node and reach `Running`:

```bash
kubectl get pods -n tpu-share -o wide
kubectl get resourceclaims -n tpu-share
```

Each claim's allocation carries a distinct `shareID`, and in integer mode a
`consumedCapacity` of one share:

```bash
kubectl get resourceclaims -n tpu-share -o yaml | grep -E 'shareID|consumedCapacity' -A2
```

Scaling past the cap leaves the extra pods `Pending`, which is the cap working:

```bash
kubectl scale deployment capped-sharing -n tpu-share --replicas=3
kubectl get pods -n tpu-share
```

## Gotcha: do not request `shares` in unlimited mode

In `unlimited` mode the driver publishes no `shares` capacity. The scheduler
skips any device that does not publish a capacity a claim asks for, so a claim
with `capacity.requests.shares` will stay `Pending` and **nothing will log why** —
the claim never reaches the driver. Use `integer-sharing.yaml` if you want to
request shares explicitly.

## Cleaning up

```bash
kubectl delete namespace tpu-share
```
