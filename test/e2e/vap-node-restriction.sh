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

# End-to-end test for the ResourceSlice node-restriction
# ValidatingAdmissionPolicy shipped in the helm chart.
#
# The driver publishes ResourceSlices via resource.k8s.io/v1. A policy that
# only lists the beta versions is inert on clusters where those versions are
# not served (the default since v1 went GA in Kubernetes 1.34), so the policy
# must match v1 as well. This test creates a kind cluster, installs the chart
# (which brings the policy, binding, ServiceAccount and RBAC), and then
# impersonates the driver ServiceAccount with the
# authentication.kubernetes.io/node-name extra that ServiceAccountTokenPodNodeInfo
# would normally stamp, and asserts:
#   1. Creating a ResourceSlice for the user's own node is allowed.
#   2. Creating a ResourceSlice for a different node is denied.
#   3. Deleting another node's ResourceSlice is denied (DELETE is validated
#      against oldObject).
#   4. Deleting the user's own ResourceSlice is allowed.
#   5. A request without a node association is denied.
#
# No driver image is needed: the ServiceAccount is exercised directly through
# kubectl impersonation, which requires no kubelet or GKE infrastructure.
#
# Usage: make test-e2e-vap   (or run this script directly)
# Knobs: KIND_K8S_TAG, KIND_IMAGE, BUILD_KIND_IMAGE, KIND_CLUSTER_NAME,
#        CONTAINER_TOOL, KIND, HELM, KUBECTL.

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"
PROJECT_DIR="$(cd -- "${CURRENT_DIR}/../.." &> /dev/null && pwd)"

set -eu
set -o pipefail

# Reuse the demo's CONTAINER_TOOL / KIND / KIND_IMAGE / BUILD_KIND_IMAGE
# defaults so docker and podman behave the same here as in the kind demo.
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-tpu-vap-e2e}"
CONTAINER_TOOL="${CONTAINER_TOOL:-}"
source "${PROJECT_DIR}/demo/clusters/kind/scripts/common.sh"

: ${HELM:="helm"}
: ${KUBECTL:="kubectl"}

WORKER_NODE="${KIND_CLUSTER_NAME}-worker"
OTHER_NODE="${KIND_CLUSTER_NAME}-control-plane"
HELM_RELEASE="tpu-dra-e2e"
HELM_NAMESPACE="dra-driver-google-tpu"

cleanup() {
    ${KIND} delete cluster --name "${KIND_CLUSTER_NAME}" || true
}
trap cleanup EXIT INT TERM

# Published kindest/node images are used by default; building one from a
# Kubernetes tag is opt-in like in the demo scripts.
if [[ "${BUILD_KIND_IMAGE}" == "true" ]]; then
    ${KIND} build node-image "${KIND_K8S_TAG}" --image "${KIND_IMAGE}"
fi

${KIND} create cluster \
    --name "${KIND_CLUSTER_NAME}" \
    --image "${KIND_IMAGE}" \
    --config "${CURRENT_DIR}/vap-kind-cluster-config.yaml" \
    --wait 2m

# The chart's DaemonSet only schedules on GKE-labeled TPU nodes, so installing
# it on a plain kind cluster deploys just the policy, RBAC and ServiceAccount.
${HELM} install "${HELM_RELEASE}" "${PROJECT_DIR}/deployments/helm/dra-driver-google-tpu" \
    --namespace "${HELM_NAMESPACE}" \
    --create-namespace

SA_NAME="$(${KUBECTL} get serviceaccount -n "${HELM_NAMESPACE}" -l app.kubernetes.io/name=dra-driver-google-tpu -o jsonpath='{.items[0].metadata.name}')"
SA_USER="system:serviceaccount:${HELM_NAMESPACE}:${SA_NAME}"

# Impersonate the driver ServiceAccount as it would appear when running in a
# pod on the worker node with ServiceAccountTokenPodNodeInfo enabled.
as_driver_on_worker() {
    ${KUBECTL} --as "${SA_USER}" \
        --as-user-extra "authentication.kubernetes.io/node-name=${WORKER_NODE}" "$@"
}

# The same ServiceAccount without a node association. A real token always
# carries some extras (e.g. the credential id), so one is included so that
# request.userInfo.extra exists and only the node-name key is missing.
as_driver_without_node() {
    ${KUBECTL} --as "${SA_USER}" \
        --as-user-extra "authentication.kubernetes.io/credential-id=JTI=vap-e2e-test" "$@"
}

# Emits a v1 ResourceSlice manifest — the version the driver publishes.
slice_manifest() {
    local name="$1" node="$2"
    cat <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: ${name}
spec:
  driver: tpu.google.com
  nodeName: ${node}
  pool:
    name: ${node}
    generation: 1
    resourceSliceCount: 1
EOF
}

# The impersonated identity must resolve before any policy checks mean
# anything: a broken impersonation flag would silently run every request as
# the admin user or without the node extra, and the assertions below would
# then blame the policy for a harness problem.
WHOAMI_USER="$(as_driver_on_worker auth whoami -o jsonpath='{.status.userInfo.username}')"
WHOAMI_NODE="$(as_driver_on_worker auth whoami -o jsonpath='{.status.userInfo.extra.authentication\.kubernetes\.io/node-name[0]}')"
if [[ "${WHOAMI_USER}" != "${SA_USER}" || "${WHOAMI_NODE}" != "${WORKER_NODE}" ]]; then
    echo "impersonation failed: requests run as '${WHOAMI_USER}' on node '${WHOAMI_NODE}', expected '${SA_USER}' on '${WORKER_NODE}'"
    exit 1
fi

# Newly created policies take a moment to become active in the apiserver.
# Wait until a request without a node association is denied by the policy
# itself (any other failure, e.g. RBAC not yet propagated, keeps waiting).
policy_active=false
for _ in $(seq 1 30); do
    if output="$(slice_manifest vap-e2e-canary "${WORKER_NODE}" | as_driver_without_node create -f - 2>&1)"; then
        ${KUBECTL} delete resourceslice vap-e2e-canary
    elif [[ "${output}" == *"no node association"* ]]; then
        policy_active=true
        break
    else
        echo "waiting for policy: ${output}"
    fi
    sleep 2
done
if [[ "${policy_active}" != "true" ]]; then
    echo "policy never became active: requests without a node association are still not denied by it"
    exit 1
fi

FAILURES=0

expect_allowed() {
    local desc="$1"; shift
    if output="$("$@" 2>&1)"; then
        echo "PASS: ${desc}"
    else
        echo "FAIL: ${desc} — expected success, got:"
        echo "${output}"
        FAILURES=$((FAILURES + 1))
    fi
}

expect_denied() {
    local desc="$1" want="$2"; shift 2
    if output="$("$@" 2>&1)"; then
        echo "FAIL: ${desc} — expected denial, but the request succeeded"
        FAILURES=$((FAILURES + 1))
    elif [[ "${output}" == *"${want}"* ]]; then
        echo "PASS: ${desc}"
    else
        echo "FAIL: ${desc} — request failed for the wrong reason:"
        echo "${output}"
        FAILURES=$((FAILURES + 1))
    fi
}

create_as_driver_on_worker() {
    slice_manifest "$1" "$2" | as_driver_on_worker create -f -
}

create_as_driver_without_node() {
    slice_manifest "$1" "$2" | as_driver_without_node create -f -
}

# 1. The driver may publish a slice for its own node.
expect_allowed "create ResourceSlice for own node" \
    create_as_driver_on_worker vap-e2e-own-node "${WORKER_NODE}"

# 2. The driver may not publish a slice for another node.
expect_denied "create ResourceSlice for another node" "may not modify" \
    create_as_driver_on_worker vap-e2e-other-node "${OTHER_NODE}"

# 3. The driver may not delete another node's slice (DELETE is checked
#    against oldObject). The slice is created by the admin user first.
slice_manifest vap-e2e-admin-owned "${OTHER_NODE}" | ${KUBECTL} create -f -
expect_denied "delete another node's ResourceSlice" "may not modify" \
    as_driver_on_worker delete resourceslice vap-e2e-admin-owned

# 4. The driver may delete its own node's slice.
expect_allowed "delete ResourceSlice for own node" \
    as_driver_on_worker delete resourceslice vap-e2e-own-node

# 5. Without a node association the request is denied outright.
expect_denied "create ResourceSlice without node association" "no node association" \
    create_as_driver_without_node vap-e2e-no-node "${WORKER_NODE}"

if [[ "${FAILURES}" -ne 0 ]]; then
    echo "${FAILURES} test(s) failed"
    exit 1
fi
echo "All ValidatingAdmissionPolicy node-restriction tests passed"
