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

# End-to-end test for KEP-5004 extended resource requests.
#
# Creates a kind cluster (${K8S_VERSION}) from the demo cluster config, which
# enables the DRAExtendedResource feature gate and fakes a 4-chip
# tpu-v4-podslice worker node (node labels plus /dev/accel* device nodes),
# installs the driver via helm from the local tree, and asserts:
#   1. With deviceClass.extendedResourceName=google.com/tpu the DeviceClass
#      carries the field, and a pod requesting `google.com/tpu: 4` via
#      resources.limits (no ResourceClaim in the pod spec) runs: the scheduler
#      generates a ResourceClaim owned by the pod, allocates all 4 devices to
#      it, and the container sees the 4 /dev/accel* nodes.
#   2. The implicit name deviceclass.resource.kubernetes.io/tpu.google.com
#      also works while the explicit name is set.
#   3. A request for fewer chips than the node has (2 of 4) is bound by the
#      scheduler but rejected by the kubelet plugin (this driver only prepares
#      all-chip claims), leaving the pod in ContainerCreating with a
#      FailedPrepareDynamicResources event.
#   4. With the value unset the DeviceClass has no extendedResourceName, a
#      google.com/tpu request is unschedulable (Insufficient google.com/tpu),
#      and the implicit name still works.
#   5. The chart renders with deviceClass overridden to null.
#
# The GKE-only init/sidecar containers of the DaemonSet are removed because
# they require real GKE infrastructure.
#
# The cluster is created with a private kubeconfig so the test never touches
# ~/.kube/config or any other kind cluster on the host.

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"
PROJECT_DIR="$(cd -- "${CURRENT_DIR}/../.." &> /dev/null && pwd)"

set -ex
set -o pipefail

: ${KIND:="kind"}
: ${HELM:="helm"}
: ${KUBECTL:="kubectl"}
: ${K8S_VERSION:="v1.35.0"}
: ${KIND_CLUSTER_NAME:="tpu-extres-e2e"}
: ${KIND_IMAGE:="kindest/node:${K8S_VERSION}"}
: ${KIND_CLUSTER_CONFIG_PATH:="${PROJECT_DIR}/demo/clusters/kind/scripts/kind-cluster-config.yaml"}
: ${DRIVER_IMAGE:="tpu-dra-driver:e2e"}

WORKER_NODE="${KIND_CLUSTER_NAME}-worker"
HELM_RELEASE="tpu-dra-e2e"
HELM_NAMESPACE="dra-driver-google-tpu"
TEST_NAMESPACE="tpu-extres-e2e"
DEVICE_CLASS="tpu.google.com"
IMPLICIT_NAME="deviceclass.resource.kubernetes.io/${DEVICE_CLASS}"

export KUBECONFIG
KUBECONFIG="$(mktemp)"

cleanup() {
    ${KIND} delete cluster --name "${KIND_CLUSTER_NAME}" || true
    rm -f "${KUBECONFIG}"
}
trap cleanup EXIT

${KIND} create cluster \
    --name "${KIND_CLUSTER_NAME}" \
    --image "${KIND_IMAGE}" \
    --config "${KIND_CLUSTER_CONFIG_PATH}" \
    --kubeconfig "${KUBECONFIG}" \
    --wait 2m

# The gate must be on for every component that handles the request path.
for c in kube-apiserver kube-scheduler kube-controller-manager; do
    ${KUBECTL} get pod -n kube-system -l component=${c} -o jsonpath='{.items[0].spec.containers[0].command}' \
        | grep -q "DRAExtendedResource=true"
done
docker exec "${WORKER_NODE}" grep -q "DRAExtendedResource: true" /var/lib/kubelet/config.yaml

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
        -p '[{"op":"remove","path":"/spec/template/spec/initContainers"},{"op":"remove","path":"/spec/template/spec/containers/2"}]' || true
    ${KUBECTL} rollout status -n "${HELM_NAMESPACE}" daemonset --timeout=180s
    # Wait for the ResourceSlice with the 4 fake chips.
    for _ in $(seq 1 45); do
        [ "$(${KUBECTL} get resourceslices -o jsonpath='{.items[?(@.spec.driver=="tpu.google.com")].spec.devices[*].name}' | wc -w | tr -d " ")" = "4" ] && return 0
        sleep 2
    done
    echo "driver did not publish 4 devices"; return 1
}

# extres_pod NAME RESOURCE COUNT: create a pod requesting COUNT of RESOURCE
# via resources.limits, with no resourceClaims in the pod spec.
extres_pod() {
    ${KUBECTL} apply -f - <<PODEOF
apiVersion: v1
kind: Pod
metadata:
  namespace: ${TEST_NAMESPACE}
  name: $1
spec:
  containers:
  - name: ctr0
    image: busybox:latest
    command: ["sleep", "infinity"]
    resources:
      limits:
        $2: $3
PODEOF
}

scheduled_message() {
    ${KUBECTL} get pod -n "${TEST_NAMESPACE}" "$1" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].message}'
}

${KUBECTL} create namespace "${TEST_NAMESPACE}"

# --- 1. Explicit mapping: google.com/tpu request runs end to end. ---
install_driver --set deviceClass.extendedResourceName=google.com/tpu
[ "$(${KUBECTL} get deviceclass ${DEVICE_CLASS} -o jsonpath='{.spec.extendedResourceName}')" = "google.com/tpu" ]

extres_pod explicit-pod google.com/tpu 4
${KUBECTL} wait -n "${TEST_NAMESPACE}" pod/explicit-pod --for=condition=Ready --timeout=180s
[ -z "$(${KUBECTL} get pod -n "${TEST_NAMESPACE}" explicit-pod -o jsonpath='{.spec.resourceClaims}')" ]

claim=$(${KUBECTL} get pod -n "${TEST_NAMESPACE}" explicit-pod -o jsonpath='{.status.extendedResourceClaimStatus.resourceClaimName}')
[ -n "${claim}" ]
[ "$(${KUBECTL} get pod -n "${TEST_NAMESPACE}" explicit-pod -o jsonpath='{.status.extendedResourceClaimStatus.requestMappings[0].resourceName}')" = "google.com/tpu" ]
[ "$(${KUBECTL} get resourceclaim -n "${TEST_NAMESPACE}" "${claim}" -o jsonpath='{.metadata.annotations.resource\.kubernetes\.io/extended-resource-claim}')" = "true" ]
[ "$(${KUBECTL} get resourceclaim -n "${TEST_NAMESPACE}" "${claim}" -o jsonpath='{.metadata.ownerReferences[0].name}')" = "explicit-pod" ]
[ "$(${KUBECTL} get resourceclaim -n "${TEST_NAMESPACE}" "${claim}" -o jsonpath='{.spec.devices.requests[0].exactly.deviceClassName}')" = "${DEVICE_CLASS}" ]
[ "$(${KUBECTL} get resourceclaim -n "${TEST_NAMESPACE}" "${claim}" -o jsonpath='{.status.allocation.devices.results[*].device}' | tr ' ' '\n' | sort | tr '\n' ' ')" = "accel0 accel1 accel2 accel3 " ]
[ "$(${KUBECTL} get resourceclaim -n "${TEST_NAMESPACE}" "${claim}" -o jsonpath='{.status.reservedFor[0].name}')" = "explicit-pod" ]
[ "$(${KUBECTL} exec -n "${TEST_NAMESPACE}" explicit-pod -- sh -c 'ls /dev | grep -c ^accel')" = "4" ]
${KUBECTL} exec -n "${TEST_NAMESPACE}" explicit-pod -- sh -c 'env | grep -q ^TPU_ACCELERATOR_TYPE='
${KUBECTL} delete pod -n "${TEST_NAMESPACE}" explicit-pod --wait=true

# --- 2. The implicit name works alongside the explicit one. ---
extres_pod implicit-pod "${IMPLICIT_NAME}" 4
${KUBECTL} wait -n "${TEST_NAMESPACE}" pod/implicit-pod --for=condition=Ready --timeout=180s
[ "$(${KUBECTL} get pod -n "${TEST_NAMESPACE}" implicit-pod -o jsonpath='{.status.extendedResourceClaimStatus.requestMappings[0].resourceName}')" = "${IMPLICIT_NAME}" ]
${KUBECTL} delete pod -n "${TEST_NAMESPACE}" implicit-pod --wait=true

# --- 3. Partial request: scheduled, but rejected by the kubelet plugin. ---
extres_pod partial-pod google.com/tpu 2
for _ in $(seq 1 60); do
    ${KUBECTL} get events -n "${TEST_NAMESPACE}" --field-selector involvedObject.name=partial-pod,reason=FailedPrepareDynamicResources -o jsonpath='{.items[*].message}' \
        | grep -q "claim requests partial tpu devices (2), only requests for all tpu devices (4)" && break
    sleep 2
done
${KUBECTL} get events -n "${TEST_NAMESPACE}" --field-selector involvedObject.name=partial-pod,reason=FailedPrepareDynamicResources -o jsonpath='{.items[*].message}' \
    | grep -q "claim requests partial tpu devices (2), only requests for all tpu devices (4)"
[ "$(${KUBECTL} get pod -n "${TEST_NAMESPACE}" partial-pod -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}')" = "ContainerCreating" ]
${KUBECTL} delete pod -n "${TEST_NAMESPACE}" partial-pod --wait=true

# --- 4. Value unset: no field, explicit name unschedulable, implicit works. ---
install_driver
[ -z "$(${KUBECTL} get deviceclass ${DEVICE_CLASS} -o jsonpath='{.spec.extendedResourceName}')" ]

extres_pod explicit-unset-pod google.com/tpu 4
for _ in $(seq 1 30); do
    scheduled_message explicit-unset-pod | grep -q "Insufficient google.com/tpu" && break
    sleep 2
done
scheduled_message explicit-unset-pod | grep -q "Insufficient google.com/tpu"
${KUBECTL} delete pod -n "${TEST_NAMESPACE}" explicit-unset-pod --wait=false

extres_pod implicit-unset-pod "${IMPLICIT_NAME}" 4
${KUBECTL} wait -n "${TEST_NAMESPACE}" pod/implicit-unset-pod --for=condition=Ready --timeout=180s
${KUBECTL} delete pod -n "${TEST_NAMESPACE}" implicit-unset-pod --wait=true

# --- 5. Chart renders with deviceClass nulled out. ---
${HELM} template "${HELM_RELEASE}" "${PROJECT_DIR}/deployments/helm/dra-driver-google-tpu" \
    --namespace "${HELM_NAMESPACE}" --set deviceClass=null -s templates/deviceclass.yaml \
    | grep -q "name: ${DEVICE_CLASS}"

echo "extended-resource e2e: all checks passed"
