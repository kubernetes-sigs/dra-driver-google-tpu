#!/usr/bin/env bash

# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# End-to-end test for KEP-4817 ResourceClaim device status publishing.
#
# Creates a kind cluster with DRAResourceClaimDeviceStatus and
# DRAResourceClaimGranularStatusAuthorization enabled, fakes a 4-chip
# tpu-v4-podslice node the same way the kind demo does (node labels plus
# /dev/accel* device nodes), builds the driver image from this checkout,
# installs it via helm, and asserts:
#   1. deviceStatus=false: the driver ServiceAccount cannot update
#      resourceclaims/status, a pod runs, and status.devices stays empty.
#   2. deviceStatus=true: the ServiceAccount can update resourceclaims/status,
#      and a pod claiming all 4 chips gets one status.devices entry per chip
#      with {type:"tpu", uuid, tpuGen, index} matching the ResourceSlice.
#   3. Repeated Prepare for an already prepared claim (the kubelet re-drives
#      NodePrepareResources after losing its DRA state) does not write again:
#      neither from the plugin's in-memory record of published claims, nor —
#      after a plugin restart clears that record — when the fetched claim
#      already carries the same entries.
#   4. The 1.36+ granular authorization: a node-associated driver identity may
#      write its entries only with the chart's resourceclaims/driver rule, and
#      the same identity without a node association is refused.
#   5. Merge: on a claim that also holds another driver's device (the
#      dra-example-driver, if its chart is available), this driver's entries
#      are written next to the foreign entry, not over it.
#   6. Deallocating the claim prunes status.devices.
#
# The GKE-only init/sidecar containers of the DaemonSet are removed because
# they require real GKE infrastructure. The script uses a private kubeconfig
# and never touches ~/.kube/config, so concurrently created kind clusters
# cannot hijack it.
#
# Usage: make test-e2e-device-status   (or run this script directly)
# Knobs: KIND, HELM, KUBECTL, K8S_VERSION, KIND_IMAGE, KIND_CLUSTER_NAME,
#        DRIVER_IMAGE, EXAMPLE_DRIVER_CHART, EXAMPLE_DRIVER_IMAGE_TAG,
#        KEEP_CLUSTER=true.

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"
PROJECT_DIR="$(cd -- "${CURRENT_DIR}/../.." &> /dev/null && pwd)"

set -eu
set -o pipefail

: ${KIND:="kind"}
: ${HELM:="helm"}
: ${KUBECTL:="kubectl"}
: ${K8S_VERSION:="v1.36.1"}
: ${KIND_CLUSTER_NAME:="tpu-kep4817-e2e"}
: ${KIND_IMAGE:="kindest/node:${K8S_VERSION}"}
: ${DRIVER_IMAGE:="tpu-dra-driver:kep4817-e2e"}
: ${KEEP_CLUSTER:="false"}
# A second, real DRA driver is needed for the merge check (status.devices
# entries must refer to allocated devices, and the kubelet only prepares a
# claim once every driver on it is registered). The dra-example-driver chart
# is looked up in the usual sibling checkout locations.
: ${EXAMPLE_DRIVER_CHART:=""}
: ${EXAMPLE_DRIVER_IMAGE_TAG:="v0.4.0"}

WORKER_NODE="${KIND_CLUSTER_NAME}-worker"
HELM_RELEASE="tpu-dra-e2e"
HELM_NAMESPACE="dra-driver-google-tpu"
TEST_NAMESPACE="tpu-test"
EXAMPLE_NAMESPACE="dra-example-driver"

TMP_DIR="$(mktemp -d)"
export KUBECONFIG="${TMP_DIR}/kubeconfig"

k() { ${KUBECTL} "$@"; }
h() { ${HELM} "$@"; }

cleanup() {
    if [[ "${KEEP_CLUSTER}" == "true" ]]; then
        echo "KEEP_CLUSTER=true: leaving cluster ${KIND_CLUSTER_NAME} running (kubeconfig: ${KUBECONFIG})"
        return
    fi
    ${KIND} delete cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" || true
    rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

FAILURES=0
check() {
    # Usage: check <description> <got> <want>
    if [[ "$2" == "$3" ]]; then
        echo "PASS: $1"
    else
        echo "FAIL: $1"
        echo "  got:  $2"
        echo "  want: $3"
        FAILURES=$((FAILURES + 1))
    fi
}

plugin_pod() { k get pods -n "${HELM_NAMESPACE}" -o name | head -1; }
plugin_logs() { k logs -n "${HELM_NAMESPACE}" "$(plugin_pod)" -c tpu-dra-plugin "$@"; }
plugin_log_count() { plugin_logs | grep -cE "$1" || true; }

# When this driver last wrote the claim's status, as recorded by the API
# server in managedFields (manager = binary name of the plugin).
tpu_status_write_time() {
    k get resourceclaim -n "${TEST_NAMESPACE}" "$1" --show-managed-fields -o json \
        | jq -r '[.metadata.managedFields[] | select(.manager == "tpu-dra-kubeletplugin" and .subresource == "status") | .time] | join(",")'
}

# JSON summary of this driver's status.devices entries on a claim:
# [{device, pool, data:{index,tpuGen,type,uuid}}] sorted, keys sorted.
claim_tpu_statuses() {
    k get resourceclaim -n "${TEST_NAMESPACE}" "$1" -o json \
        | jq -S -c '[.status.devices[]? | select(.driver == "tpu.google.com") | {device, pool, data}] | sort_by(.device)'
}

claim_status_drivers() {
    k get resourceclaim -n "${TEST_NAMESPACE}" "$1" -o json \
        | jq -S -c '[.status.devices[]?.driver] | group_by(.) | map({(.[0]): length}) | add // {}'
}

# The kubelet forgets which claims it prepared and restarts, so the next pod
# using an already prepared claim makes it call NodePrepareResources again.
kubelet_forget_prepared_claims() {
    docker exec "${WORKER_NODE}" bash -c \
        'systemctl stop kubelet && rm -f /var/lib/kubelet/dra_manager_state && systemctl start kubelet'
    sleep 5
    k wait node/"${WORKER_NODE}" --for=condition=Ready --timeout=120s >/dev/null
}

restart_plugin() {
    k delete -n "${HELM_NAMESPACE}" "$(plugin_pod)" --wait=true >/dev/null
    k rollout status -n "${HELM_NAMESPACE}" daemonset --timeout=180s >/dev/null
    sleep 5
}

# ---------------------------------------------------------------- cluster ---
if ${KIND} get clusters | grep -qx "${KIND_CLUSTER_NAME}"; then
    ${KIND} get kubeconfig --name "${KIND_CLUSTER_NAME}" > "${KUBECONFIG}"
else
    ${KIND} create cluster \
        --name "${KIND_CLUSTER_NAME}" \
        --image "${KIND_IMAGE}" \
        --config "${CURRENT_DIR}/device-status-kind-cluster-config.yaml" \
        --kubeconfig "${KUBECONFIG}" \
        --wait 2m
fi

echo "== cluster: ${KIND_CLUSTER_NAME} $(k version -o json | jq -r .serverVersion.gitVersion)"
k get --raw /metrics | grep -E 'kubernetes_feature_enabled\{name="DRAResourceClaim(DeviceStatus|GranularStatusAuthorization)"'
check "resource.k8s.io/v1 is the served version (the driver's V1beta2 guard is not the path taken)" \
    "$(k get --raw /apis/resource.k8s.io | jq -c '[.versions[].version]')" '["v1"]'

# Fake the TPU devices of a 4-chip v4 node (the driver discovers
# /dev/accel[0-9]* for tpu-v4-podslice).
for i in 0 1 2 3; do
    docker exec "${WORKER_NODE}" bash -c "mknod -m 666 /dev/accel${i} b 100 ${i} 2>/dev/null || true"
done

docker build -f "${PROJECT_DIR}/deployments/container/Dockerfile" -t "${DRIVER_IMAGE}" "${PROJECT_DIR}"
${KIND} load docker-image "${DRIVER_IMAGE}" --name "${KIND_CLUSTER_NAME}"

install_driver() {
    # Usage: install_driver [extra helm --set args...]
    h upgrade -i --create-namespace --namespace "${HELM_NAMESPACE}" "${HELM_RELEASE}" \
        "${PROJECT_DIR}/deployments/helm/dra-driver-google-tpu" \
        --set image.repository="docker.io/library/${DRIVER_IMAGE%%:*}" \
        --set image.tag="${DRIVER_IMAGE##*:}" \
        --set image.pullPolicy=Never \
        --set kubeletPlugin.priorityClassName="" \
        "$@" >/dev/null
    # The GKE-only init container and vbar sidecar need real GKE infrastructure.
    local ds
    ds=$(k get ds -n "${HELM_NAMESPACE}" -o name | head -1)
    k patch -n "${HELM_NAMESPACE}" "${ds}" --type=json \
        -p '[{"op":"remove","path":"/spec/template/spec/initContainers"},{"op":"remove","path":"/spec/template/spec/containers/2"}]' >/dev/null
    k rollout status -n "${HELM_NAMESPACE}" daemonset --timeout=180s >/dev/null
    # Wait for the ResourceSlice with the 4 fake chips.
    for _ in $(seq 1 45); do
        [[ "$(k get resourceslices -o json | jq '[.items[] | select(.spec.driver == "tpu.google.com") | .spec.devices[]] | length')" == "4" ]] && return
        sleep 2
    done
    echo "driver never published its 4 devices"; exit 1
}

driver_sa() {
    echo "system:serviceaccount:${HELM_NAMESPACE}:$(k get sa -n "${HELM_NAMESPACE}" -l app.kubernetes.io/name=dra-driver-google-tpu -o jsonpath='{.items[0].metadata.name}')"
}

k create namespace "${TEST_NAMESPACE}" --dry-run=client -o yaml | k apply -f - >/dev/null

# A pod using a standalone (shareable) claim.
pod_manifest() {
    # Usage: pod_manifest <pod name> <claim name>
    cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  namespace: ${TEST_NAMESPACE}
  name: $1
spec:
  containers:
  - name: ctr
    image: registry.k8s.io/pause:3.10
    resources:
      claims:
      - name: c
  resourceClaims:
  - name: c
    resourceClaimName: $2
YAML
}

# A claim for all 4 chips, optionally plus one device of another class.
claim_manifest() {
    # Usage: claim_manifest <claim name> [extra device class]
    cat <<YAML
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  namespace: ${TEST_NAMESPACE}
  name: $1
spec:
  devices:
    requests:
    - name: tpus
      exactly:
        deviceClassName: tpu.google.com
        allocationMode: All
YAML
    if [[ -n "${2:-}" ]]; then
        cat <<YAML
    - name: other
      exactly:
        deviceClassName: $2
YAML
    fi
}

run_pod() {
    # Usage: run_pod <pod name> <claim name>
    pod_manifest "$1" "$2" | k apply -f - >/dev/null
    k wait -n "${TEST_NAMESPACE}" "pod/$1" --for=condition=Ready --timeout=180s >/dev/null
    sleep 3
}

wait_deallocated() {
    # Usage: wait_deallocated <claim name>; prints {alloc,devices}.
    local got=""
    for _ in $(seq 1 45); do
        got=$(k get resourceclaim -n "${TEST_NAMESPACE}" "$1" -o json | jq -c '{alloc: (.status.allocation != null), devices: (.status.devices // [] | length)}')
        [[ "${got}" == '{"alloc":false,"devices":0}' ]] && break
        sleep 2
    done
    echo "${got}"
}

# ------------------------------------------------ 1. deviceStatus=false ---
echo "== 1. deviceStatus=false"
install_driver --set deviceStatus=false
check "deviceStatus=false: driver SA cannot update resourceclaims/status" \
    "$(k auth can-i update resourceclaims --subresource=status --as "$(driver_sa)" -n "${TEST_NAMESPACE}")" "no"
check "deviceStatus=false: DEVICE_STATUS env is \"false\"" \
    "$(k get ds -n "${HELM_NAMESPACE}" -o json | jq -r '.items[0].spec.template.spec.containers[0].env[] | select(.name=="DEVICE_STATUS") | .value')" "false"

claim_manifest off-claim | k apply -f - >/dev/null
run_pod off-pod off-claim
check "deviceStatus=false: pod runs and status.devices stays empty" \
    "$(k get resourceclaim -n "${TEST_NAMESPACE}" off-claim -o jsonpath='{.status.devices}')" ""
k delete pod -n "${TEST_NAMESPACE}" off-pod --wait=true >/dev/null
k delete resourceclaim -n "${TEST_NAMESPACE}" off-claim --wait=true >/dev/null

# ------------------------------------------------- 2. deviceStatus=true ---
echo "== 2. deviceStatus=true"
install_driver --set deviceStatus=true
check "deviceStatus=true: driver SA can update resourceclaims/status" \
    "$(k auth can-i update resourceclaims --subresource=status --as "$(driver_sa)" -n "${TEST_NAMESPACE}")" "yes"
check "deviceStatus=true: DEVICE_STATUS env is \"true\"" \
    "$(k get ds -n "${HELM_NAMESPACE}" -o json | jq -r '.items[0].spec.template.spec.containers[0].env[] | select(.name=="DEVICE_STATUS") | .value')" "true"

claim_manifest shared | k apply -f - >/dev/null
run_pod pod-a shared

# What the driver should have published: one entry per device, with the
# ResourceSlice attributes packed as {type,uuid,tpuGen,index}.
expected_statuses=$(k get resourceslices -o json | jq -S -c '
    [.items[] | select(.spec.driver == "tpu.google.com") | .spec.pool.name as $pool | .spec.devices[]
     | {device: .name, pool: $pool,
        data: {type: "tpu", uuid: .attributes.uuid.string, tpuGen: .attributes.tpuGen.string, index: .attributes.index.int}}]
    | sort_by(.device)')
got_statuses=""
for _ in $(seq 1 30); do
    got_statuses=$(claim_tpu_statuses shared)
    [[ "${got_statuses}" == "${expected_statuses}" ]] && break
    sleep 2
done
echo "  status.devices: ${got_statuses}"
check "status.devices carries {type,uuid,tpuGen,index} for all 4 chips, matching the ResourceSlice" \
    "${got_statuses}" "${expected_statuses}"
check "exactly 4 entries, all owned by tpu.google.com" "$(claim_status_drivers shared)" '{"tpu.google.com":4}'
check "plugin logged exactly one publish for the claim" \
    "$(plugin_log_count 'Publishing device status for 4 devices to ResourceClaim tpu-test/shared')" "1"
first_write=$(tpu_status_write_time shared)
check "the API server attributes the status write to the plugin" "$([[ -n "${first_write}" ]] && echo attributed)" "attributed"

# ----------------------------------- 3. repeated prepare: no extra writes ---
echo "== 3. repeated Prepare for the same claim"
# 3a. Kubelet re-drives NodePrepareResources; the plugin remembers it already
#     published and does not even fetch the claim.
kubelet_forget_prepared_claims
run_pod pod-b shared
check "3a: kubelet re-drove Prepare (plugin took the checkpoint path)" "$(plugin_log_count 'skip prepare: claim .* already exists in checkpoint')" "1"
check "3a: no second publish attempt (published-claim record)" "$(plugin_log_count 'Publishing device status')" "1"
check "3a: no new status write by the plugin" "$(tpu_status_write_time shared)" "${first_write}"

# 3b. Plugin restart clears the record; the plugin re-fetches, finds its
#     entries already present and skips the write.
restart_plugin
kubelet_forget_prepared_claims
run_pod pod-c shared
check "3b: kubelet re-drove Prepare after plugin restart" "$(plugin_log_count 'skip prepare: claim .* already exists in checkpoint')" "1"
check "3b: plugin attempted to publish (record was cleared by the restart)" "$(plugin_log_count 'Publishing device status')" "1"
check "3b: no publish failure logged" "$(plugin_log_count 'Failed to update device status')" "0"
check "3b: no new status write by the plugin (fetched claim already up to date)" "$(tpu_status_write_time shared)" "${first_write}"
check "3b: status.devices unchanged" "$(claim_tpu_statuses shared)" "${expected_statuses}"

# ------------------------------------------- 4. granular authorization ---
echo "== 4. granular status authorization (resourceclaims/driver)"
# Impersonate the driver ServiceAccount as it appears from a pod on the
# worker (ServiceAccountTokenPodNodeInfo stamps the node-name extra), and PUT
# the status the way the plugin does (Get + UpdateStatus, no GET on the
# status subresource).
as_driver_on_worker() {
    k --as "$(driver_sa)" --as-user-extra "authentication.kubernetes.io/node-name=${WORKER_NODE}" "$@"
}
as_driver_without_node() {
    k --as "$(driver_sa)" --as-user-extra "authentication.kubernetes.io/credential-id=JTI=device-status-e2e" "$@"
}
k get resourceclaim -n "${TEST_NAMESPACE}" shared -o json > "${TMP_DIR}/shared.json"
jq '.status.devices = []' "${TMP_DIR}/shared.json" > "${TMP_DIR}/shared-empty.json"
check "4: node-associated driver identity may write its status entries" \
    "$(as_driver_on_worker replace --subresource=status -f "${TMP_DIR}/shared-empty.json" >/dev/null 2>&1 && echo allowed || echo denied)" "allowed"
k get resourceclaim -n "${TEST_NAMESPACE}" shared -o json | jq --slurpfile f "${TMP_DIR}/shared.json" '.status.devices = $f[0].status.devices' > "${TMP_DIR}/shared-restore.json"
as_driver_on_worker replace --subresource=status -f "${TMP_DIR}/shared-restore.json" >/dev/null
k get resourceclaim -n "${TEST_NAMESPACE}" shared -o json | jq '.status.devices = []' > "${TMP_DIR}/shared-empty.json"
output=$(as_driver_without_node replace --subresource=status -f "${TMP_DIR}/shared-empty.json" 2>&1 || true)
check "4: the same identity without a node association is refused by the granular authorizer" \
    "$([[ "${output}" == *'requires resource="resourceclaims/driver"'* ]] && echo refused || echo "${output}")" "refused"
# Remove the chart's granular rule: the node-associated identity is refused too.
clusterrole=$(k get clusterrole -o name | grep "${HELM_RELEASE}")
k get "${clusterrole}" -o json | jq '.rules |= map(select(.resources != ["resourceclaims/driver"]))' | k replace -f - >/dev/null
sleep 3
output=$(as_driver_on_worker replace --subresource=status -f "${TMP_DIR}/shared-empty.json" 2>&1 || true)
check "4: without the resourceclaims/driver rule the node-associated identity is refused" \
    "$([[ "${output}" == *'cannot associated-node:update resource "resourceclaims/driver"'* ]] && echo refused || echo "${output}")" "refused"
# Restore the chart's RBAC. Helm 4 applies server-side and must take back
# ownership of .rules from kubectl; Helm 3 merges and needs no flag.
force_conflicts=""
if h version --short 2>/dev/null | grep -q '^v4'; then force_conflicts="--force-conflicts"; fi
install_driver --set deviceStatus=true ${force_conflicts}
sleep 3
k get resourceclaim -n "${TEST_NAMESPACE}" shared -o json | jq '.status.devices = []' > "${TMP_DIR}/shared-empty.json"
check "4: rule restored by the chart, write allowed again" \
    "$(as_driver_on_worker replace --subresource=status -f "${TMP_DIR}/shared-empty.json" >/dev/null 2>&1 && echo allowed || echo denied)" "allowed"
k get resourceclaim -n "${TEST_NAMESPACE}" shared -o json | jq --slurpfile f "${TMP_DIR}/shared.json" '.status.devices = $f[0].status.devices' > "${TMP_DIR}/shared-restore.json"
as_driver_on_worker replace --subresource=status -f "${TMP_DIR}/shared-restore.json" >/dev/null

# ------------------------------------------ 6a. pruned on deallocation ---
echo "== 6a. deallocation prunes status.devices"
k delete pod -n "${TEST_NAMESPACE}" pod-a pod-b pod-c --wait=true >/dev/null
check "claim deallocated and status.devices pruned" "$(wait_deallocated shared)" '{"alloc":false,"devices":0}'
k delete resourceclaim -n "${TEST_NAMESPACE}" shared --wait=true >/dev/null

# ------------------------------------ 5. merge with another driver's entry ---
echo "== 5. merge next to another driver's status entry"
if [[ -z "${EXAMPLE_DRIVER_CHART}" ]]; then
    for candidate in \
        "${PROJECT_DIR}/../dra-example-driver" \
        "${PROJECT_DIR}/../../dra-example-driver" \
        "${PROJECT_DIR}/../../../dra-example-driver"; do
        if [[ -d "${candidate}/deployments/helm/dra-example-driver" ]]; then
            EXAMPLE_DRIVER_CHART="$(cd "${candidate}/deployments/helm/dra-example-driver" && pwd)"
            break
        fi
    done
fi
if [[ -z "${EXAMPLE_DRIVER_CHART}" ]]; then
    echo "SKIP: dra-example-driver chart not found (set EXAMPLE_DRIVER_CHART); merge check needs a second DRA driver"
    FAILURES=$((FAILURES + 1))
else
    example_image="registry.k8s.io/dra-example-driver/dra-example-driver:${EXAMPLE_DRIVER_IMAGE_TAG}"
    if docker image inspect "${example_image}" >/dev/null 2>&1; then
        ${KIND} load docker-image "${example_image}" --name "${KIND_CLUSTER_NAME}"
    fi
    h upgrade -i --create-namespace --namespace "${EXAMPLE_NAMESPACE}" dra-example-driver "${EXAMPLE_DRIVER_CHART}" \
        --set image.tag="${EXAMPLE_DRIVER_IMAGE_TAG}" --set image.pullPolicy=IfNotPresent \
        --set kubeletPlugin.numDevices=2 --set gpuDeviceStatus=false >/dev/null
    k rollout status -n "${EXAMPLE_NAMESPACE}" daemonset --timeout=180s >/dev/null
    for _ in $(seq 1 45); do
        [[ "$(k get resourceslices -o json | jq '[.items[] | select(.spec.driver == "gpu.example.com")] | length')" != "0" ]] && break
        sleep 2
    done

    claim_manifest mixed gpu.example.com | k apply -f - >/dev/null
    run_pod mixed-pod-1 mixed
    check "5: mixed claim allocated devices from both drivers" \
        "$(k get resourceclaim -n "${TEST_NAMESPACE}" mixed -o json | jq -c '[.status.allocation.devices.results[].driver] | unique')" \
        '["gpu.example.com","tpu.google.com"]'
    check "5: TPU plugin published its 4 entries on the mixed claim" "$(claim_status_drivers mixed)" '{"tpu.google.com":4}'

    # Keep the claim allocated across pod churn (a non-pod consumer), let the
    # other driver's write replace status.devices wholesale — as the reference
    # dra-example-driver does — and make the TPU plugin prepare again: the
    # pod goes away (Unprepare) and a new pod comes (fresh Prepare + publish).
    k patch resourceclaim -n "${TEST_NAMESPACE}" mixed --subresource=status --type=json \
        -p '[{"op":"add","path":"/status/reservedFor/-","value":{"resource":"configmaps","name":"e2e-holder","uid":"11111111-2222-3333-4444-555555555555"}}]' >/dev/null
    foreign_entry='{"driver":"gpu.example.com","pool":"'"${WORKER_NODE}"'","device":"gpu-0","data":{"uuid":"gpu-foreign","model":"LATEST-GPU-MODEL"}}'
    k patch resourceclaim -n "${TEST_NAMESPACE}" mixed --subresource=status --type=merge \
        -p "{\"status\":{\"devices\":[${foreign_entry}]}}" >/dev/null
    check "5: foreign entry in place, TPU entries gone" "$(claim_status_drivers mixed)" '{"gpu.example.com":1}'
    k delete pod -n "${TEST_NAMESPACE}" mixed-pod-1 --wait=true >/dev/null
    check "5: claim stays allocated after the pod (held by a non-pod consumer)" \
        "$(k get resourceclaim -n "${TEST_NAMESPACE}" mixed -o json | jq -c '{alloc: (.status.allocation != null), devices: [.status.devices[].driver]}')" \
        '{"alloc":true,"devices":["gpu.example.com"]}'
    run_pod mixed-pod-2 mixed
    check "5: TPU entries merged next to the foreign entry (not clobbered)" "$(claim_status_drivers mixed)" '{"gpu.example.com":1,"tpu.google.com":4}'
    check "5: foreign entry's data survived untouched" \
        "$(k get resourceclaim -n "${TEST_NAMESPACE}" mixed -o json | jq -S -c '.status.devices[] | select(.driver=="gpu.example.com") | .data')" \
        '{"model":"LATEST-GPU-MODEL","uuid":"gpu-foreign"}'

    # ------------------------------------------ 6b. pruned on deallocation ---
    echo "== 6b. deallocation prunes the mixed claim too"
    # Drop the non-pod holder while the pod still reserves the claim, so that
    # deleting the pod deallocates it the usual way.
    holder_index=$(k get resourceclaim -n "${TEST_NAMESPACE}" mixed -o json | jq '.status.reservedFor | map(.name == "e2e-holder") | index(true)')
    k patch resourceclaim -n "${TEST_NAMESPACE}" mixed --subresource=status --type=json \
        -p "[{\"op\":\"remove\",\"path\":\"/status/reservedFor/${holder_index}\"}]" >/dev/null
    k delete pod -n "${TEST_NAMESPACE}" mixed-pod-2 --wait=true >/dev/null
    check "mixed claim deallocated and status.devices pruned" "$(wait_deallocated mixed)" '{"alloc":false,"devices":0}'
    k delete resourceclaim -n "${TEST_NAMESPACE}" mixed --wait=true >/dev/null
fi

echo "== plugin log lines about device status:"
plugin_logs | grep -iE "device status|skip prepare" || true

if [[ "${FAILURES}" -ne 0 ]]; then
    echo "${FAILURES} check(s) failed"
    exit 1
fi
echo "KEP-4817 device status e2e: all checks passed"
