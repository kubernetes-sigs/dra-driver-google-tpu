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

# End-to-end test for KEP-5517 node-allocatable resource overhead.
#
# Creates a kind cluster (built from ${K8S_VERSION}) with the
# DRANodeAllocatableResources feature gate enabled, fakes a 4-chip
# tpu-v4-podslice node the same way the kind demo does (node labels plus
# /dev/accel* device nodes), installs the driver via helm with overhead
# values, and asserts:
#   1. Published devices carry the configured overhead entries.
#   2. A pod claiming all 4 chips runs, and the scheduler records the
#      per-device-summed overhead in pod.status.nodeAllocatableResourceClaimStatuses.
#   3. The pod-level cgroup memory limit equals spec limit + accounted overhead.
#   4. Overhead flags with the driver feature gate disabled fail startup with
#      a clear error.
#   5. With the gate disabled and no values configured, the driver starts and
#      publishes no nodeAllocatableResources field.
#
# The GKE-only init/sidecar containers of the DaemonSet are removed because
# they require real GKE infrastructure.

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"
PROJECT_DIR="$(cd -- "${CURRENT_DIR}/../.." &> /dev/null && pwd)"

set -ex
set -o pipefail

: ${KIND:="kind"}
: ${HELM:="helm"}
: ${KUBECTL:="kubectl"}
: ${K8S_VERSION:="v1.37.0-rc.0"}
: ${KIND_CLUSTER_NAME:="tpu-overhead-e2e"}
: ${KIND_IMAGE:="kindest/node:${K8S_VERSION}"}
: ${DRIVER_IMAGE:="tpu-dra-driver:e2e"}

WORKER_NODE="${KIND_CLUSTER_NAME}-worker"
HELM_RELEASE="tpu-dra-e2e"
HELM_NAMESPACE="dra-driver-google-tpu"
TEST_NAMESPACE="tpu-test"

cleanup() {
    ${KIND} delete cluster --name "${KIND_CLUSTER_NAME}" || true
}
trap cleanup EXIT

# Build a node image for ${K8S_VERSION} from its published release artifacts
# unless one is already present.
if ! docker image inspect "${KIND_IMAGE}" >/dev/null 2>&1; then
    ${KIND} build node-image "${K8S_VERSION}" --image "${KIND_IMAGE}"
fi

${KIND} create cluster \
    --name "${KIND_CLUSTER_NAME}" \
    --image "${KIND_IMAGE}" \
    --config "${CURRENT_DIR}/kind-cluster-config.yaml" \
    --wait 2m

# Fake the TPU devices of a 4-chip v4 node (the driver discovers
# /dev/accel[0-9]* for tpu-v4-podslice).
for i in 0 1 2 3; do
    docker exec "${WORKER_NODE}" bash -c "mknod -m 666 /dev/accel${i} b 100 ${i} 2>/dev/null || true"
done

docker build -f "${PROJECT_DIR}/deployments/container/Dockerfile" -t "${DRIVER_IMAGE}" "${PROJECT_DIR}"
${KIND} load docker-image "${DRIVER_IMAGE}" --name "${KIND_CLUSTER_NAME}"

install_driver() {
    # Usage: install_driver [extra helm --set args...]
    ${HELM} upgrade -i --create-namespace --namespace "${HELM_NAMESPACE}" "${HELM_RELEASE}" \
        "${PROJECT_DIR}/deployments/helm/dra-driver-google-tpu" \
        --set image.repository="docker.io/library/${DRIVER_IMAGE%%:*}" \
        --set image.tag="${DRIVER_IMAGE##*:}" \
        --set image.pullPolicy=Never \
        --set kubeletPlugin.priorityClassName="" \
        "$@"
    # The GKE-only init container and vbar sidecar need real GKE infrastructure.
    local ds
    ds=$(${KUBECTL} get ds -n "${HELM_NAMESPACE}" -o name | head -1)
    ${KUBECTL} patch -n "${HELM_NAMESPACE}" "${ds}" --type=json \
        -p '[{"op":"remove","path":"/spec/template/spec/initContainers"},{"op":"remove","path":"/spec/template/spec/containers/2"}]'
}

plugin_pod() {
    ${KUBECTL} get pods -n "${HELM_NAMESPACE}" -o name | head -1
}

# --- 1. Install with overhead values and the driver gate enabled. ---
install_driver \
    --set featureGates.NodeAllocatableResources=true \
    --set nodeAllocatableOverhead.memory.perPod=256Mi \
    --set nodeAllocatableOverhead.memory.perContainer=64Mi \
    --set nodeAllocatableOverhead.cpu.perPod=500m
${KUBECTL} rollout status -n "${HELM_NAMESPACE}" daemonset --timeout=180s

# The driver retries the GCE metadata server for ~15s before falling back to
# the node labels, then publishes.
overhead=""
for _ in $(seq 1 30); do
    overhead=$(${KUBECTL} get resourceslices -o json \
        | jq -c '[.items[].spec.devices[].nodeAllocatableResources | select(. != null)] | first // empty')
    [ -n "${overhead}" ] && break
    sleep 2
done
expected='{"cpu":{"overhead":{"perPod":"500m"}},"memory":{"overhead":{"perContainer":"64Mi","perPod":"256Mi"}}}'
[ "${overhead}" = "${expected}" ]

device_count=$(${KUBECTL} get resourceslices -o json \
    | jq '[.items[].spec.devices[] | select(.nodeAllocatableResources != null)] | length')
[ "${device_count}" = "4" ]

# --- 2. A pod claiming all 4 chips runs and the overhead is accounted. ---
${KUBECTL} apply -f "${PROJECT_DIR}/demo/specs/node-allocatable-overhead.yaml"
${KUBECTL} wait -n "${TEST_NAMESPACE}" pod/tpu-overhead-pod --for=condition=Ready --timeout=180s

claim_overhead=$(${KUBECTL} get pod -n "${TEST_NAMESPACE}" tpu-overhead-pod -o json \
    | jq -c '.status.nodeAllocatableResourceClaimStatuses[0].overhead | sort_by(.name)')
# Overhead is accounted per allocated device: 4 x 500m cpu, 4 x 256Mi / 64Mi memory.
expected_claim='[{"name":"cpu","perPod":"2"},{"name":"memory","perContainer":"256Mi","perPod":"1Gi"}]'
[ "${claim_overhead}" = "${expected_claim}" ]

# --- 3. Pod-level cgroup limit = 128Mi spec limit + 1Gi perPod + 256Mi perContainer. ---
pod_uid=$(${KUBECTL} get pod -n "${TEST_NAMESPACE}" tpu-overhead-pod -o jsonpath='{.metadata.uid}' | tr - _)
pod_memory_max=$(docker exec "${WORKER_NODE}" bash -c \
    "cat \$(find /sys/fs/cgroup -type d -name \"*pod${pod_uid}*\" | head -1)/memory.max")
[ "${pod_memory_max}" = "1476395008" ]

${KUBECTL} delete -f "${PROJECT_DIR}/demo/specs/node-allocatable-overhead.yaml" --wait=false

# --- 4. Overhead values with the driver gate disabled fail startup with a clear error. ---
install_driver --set nodeAllocatableOverhead.memory.perPod=256Mi
for _ in $(seq 1 30); do
    ${KUBECTL} logs -n "${HELM_NAMESPACE}" "$(plugin_pod)" -c tpu-dra-plugin --tail=5 2>/dev/null \
        | grep -q "feature gate NodeAllocatableResources is disabled" && break
    sleep 2
done
${KUBECTL} logs -n "${HELM_NAMESPACE}" "$(plugin_pod)" -c tpu-dra-plugin --tail=5 \
    | grep -q "feature gate NodeAllocatableResources is disabled"

# --- 5. Gate disabled with nothing configured: clean start, no field published. ---
install_driver
${KUBECTL} rollout status -n "${HELM_NAMESPACE}" daemonset --timeout=180s
sleep 20
without_field=$(${KUBECTL} get resourceslices -o json \
    | jq '[.items[].spec.devices[].nodeAllocatableResources | select(. != null)] | length')
[ "${without_field}" = "0" ]

echo "node-allocatable overhead e2e: all checks passed"
