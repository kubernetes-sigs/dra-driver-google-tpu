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

# End-to-end test for opaque device configuration (TpuConfig).
#
# Runs against a cluster where the TPU DRA driver is installed on a node with
# four fake TPU devices, as created by demo/clusters/kind. It exercises the
# whole pipeline: opaque config on a DeviceClass / ResourceClaim (unscoped,
# request-scoped, subrequest-scoped) -> strict decode -> precedence -> CDI
# env edits observed inside the workload container, plus rejection of invalid
# and conflicting configs at NodePrepareResources.
#
# Usage:
#   KUBECONFIG=... test/e2e/opaque-config-e2e.sh
#
#   CREATE_CLUSTER=true test/e2e/opaque-config-e2e.sh
#     Builds the driver image from this checkout, creates a kind cluster with a
#     private kubeconfig (never touching ~/.kube/config), installs the driver
#     via helm, runs the tests, and deletes the cluster on exit.
#
# Environment:
#   KIND_CLUSTER_NAME  name of the kind cluster to create (CREATE_CLUSTER=true)
#   KEEP_CLUSTER=true  do not delete the created cluster on exit
#   E2E_NAMESPACE      namespace used for test objects (default: tpu-e2e)

set -o errexit
set -o nounset
set -o pipefail

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"
REPO_DIR="$(cd -- "${CURRENT_DIR}/../.." &> /dev/null && pwd)"

: "${CREATE_CLUSTER:=false}"
: "${KEEP_CLUSTER:=false}"
: "${KIND_CLUSTER_NAME:=tpu-opaque-config-e2e}"
: "${E2E_NAMESPACE:=tpu-e2e}"
: "${DRIVER_NAME:=tpu.google.com}"
: "${CONFIG_API_VERSION:=tpu.resource.google.com/v1alpha1}"
: "${POD_TIMEOUT:=180s}"
# How long (seconds) to wait for the kubelet to report a failed prepare.
: "${PREPARE_FAILURE_TIMEOUT:=90}"

PASS=0
FAIL=0
FAILED_CASES=()

log()  { echo "[e2e] $*"; }
pass() { PASS=$((PASS + 1)); log "PASS: $*"; }
fail() { FAIL=$((FAIL + 1)); FAILED_CASES+=("$*"); log "FAIL: $*"; }

WORK_DIR="$(mktemp -d)"
cleanup() {
	set +e
	if [ "${CREATE_CLUSTER}" = "true" ] && [ "${KEEP_CLUSTER}" != "true" ]; then
		log "Deleting kind cluster ${KIND_CLUSTER_NAME}"
		kind delete cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}"
	fi
	rm -rf "${WORK_DIR}"
}
trap cleanup EXIT

if [ "${CREATE_CLUSTER}" = "true" ]; then
	# A private kubeconfig: kind honours $KUBECONFIG, so concurrent clusters
	# on the same machine never rewrite each other's current-context.
	export KUBECONFIG="${WORK_DIR}/kubeconfig"
	: "${REGISTRY:=localhost}"
	: "${IMAGE:=dra-driver-google-tpu}"
	: "${TAG:=e2e}"
	export REGISTRY IMAGE TAG

	log "Building driver image ${REGISTRY}/${IMAGE}:${TAG} from ${REPO_DIR}"
	"${REPO_DIR}/demo/scripts/build-driver-image.sh"

	log "Creating kind cluster ${KIND_CLUSTER_NAME} (kubeconfig: ${KUBECONFIG})"
	KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}" \
		"${REPO_DIR}/demo/clusters/kind/scripts/create-kind-cluster.sh"

	log "Loading driver image into the cluster"
	kind load docker-image "${REGISTRY}/${IMAGE}:${TAG}" --name "${KIND_CLUSTER_NAME}"

	log "Installing the driver via helm"
	helm upgrade -i --create-namespace --namespace dra-driver-google-tpu dra-driver-google-tpu \
		"${REPO_DIR}/deployments/helm/dra-driver-google-tpu" \
		--set image.repository="${REGISTRY}/${IMAGE}" \
		--set image.tag="${TAG}" \
		--set image.pullPolicy=IfNotPresent \
		--set kubeletPlugin.priorityClassName="" \
		--set 'kubeletPlugin.tolerations[0].key=google.com/tpu' \
		--set 'kubeletPlugin.tolerations[0].operator=Exists' \
		--set 'kubeletPlugin.tolerations[0].effect=NoSchedule'
fi

: "${KUBECONFIG:?KUBECONFIG must point at a cluster with the TPU DRA driver installed (or set CREATE_CLUSTER=true)}"
export KUBECONFIG

log "Using context $(kubectl config current-context)"
log "Waiting for the driver to publish a ResourceSlice"
slice_query="{.items[?(@.spec.driver=='${DRIVER_NAME}')].metadata.name}"
for _ in $(seq 1 60); do
	if kubectl get resourceslices -o jsonpath="${slice_query}" | grep -q .; then
		break
	fi
	sleep 5
done
kubectl get resourceslices -o jsonpath="${slice_query}" | grep -q . \
	|| { log "driver ${DRIVER_NAME} published no ResourceSlice"; exit 1; }

kubectl create namespace "${E2E_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# ---------------------------------------------------------------------------
# Manifest helpers
# ---------------------------------------------------------------------------

# tpu_config LEVEL [STDERR_LEVEL] -> one-line YAML for an opaque TpuConfig
# entry (without the leading "- ").
tpu_config() {
	local stderr=""
	if [ -n "${2:-}" ]; then
		stderr=", stderrLevel: $2"
	fi
	echo "opaque: {driver: ${DRIVER_NAME}, parameters: {apiVersion: ${CONFIG_API_VERSION}, kind: TpuConfig, logging: {level: $1${stderr}}}}"
}

# claim NAME REQUESTS_YAML CONFIG_YAML -> ResourceClaim manifest. REQUESTS_YAML
# and CONFIG_YAML are YAML list bodies already indented by four spaces.
claim() {
	cat <<MANIFEST
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: {namespace: ${E2E_NAMESPACE}, name: $1}
spec:
  devices:
    requests:
$2
MANIFEST
	if [ -n "$3" ]; then
		printf '    config:\n%s\n' "$3"
	fi
}

# pod NAME -> pod manifest that prints the config env vars and sleeps.
pod() {
	cat <<MANIFEST
apiVersion: v1
kind: Pod
metadata: {namespace: ${E2E_NAMESPACE}, name: $1}
spec:
  restartPolicy: Never
  tolerations: [{key: google.com/tpu, operator: Exists, effect: NoSchedule}]
  containers:
  - name: ctr
    image: busybox:latest
    command: ["/bin/sh", "-c", "echo BEGIN_ENV; env | grep -E '^TPU_(MIN|STDERR)_LOG_LEVEL=' | sort; echo END_ENV; sleep infinity"]
    resources: {claims: [{name: tpu}]}
  resourceClaims: [{name: tpu, resourceClaimName: $1}]
MANIFEST
}

ALL_TPUS='    - {name: tpus, exactly: {deviceClassName: tpu.google.com, allocationMode: All}}'
ALL_TPUS_CLASS_CONFIG='    - {name: tpus, exactly: {deviceClassName: tpu-e2e-class-config, allocationMode: All}}'
# The first subrequest can never be satisfied on a four-chip node, so the
# allocator falls through to "four", and results carry request "tpus/four".
FIRST_AVAILABLE='    - name: tpus
      firstAvailable:
      - {name: eight, deviceClassName: tpu.google.com, count: 8}
      - {name: four, deviceClassName: tpu.google.com, count: 4}'
SPLIT_REQUESTS='    - {name: a, exactly: {deviceClassName: tpu.google.com, count: 2}}
    - {name: b, exactly: {deviceClassName: tpu.google.com, count: 2}}'

teardown_case() {
	kubectl -n "${E2E_NAMESPACE}" delete pod "$1" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kubectl -n "${E2E_NAMESPACE}" delete resourceclaim "$1" --ignore-not-found --wait=true >/dev/null 2>&1 || true
}

# expect_env NAME REQUESTS CONFIG WANT_ENV
# Applies the claim and pod, waits for the pod to run, and asserts that the
# lines between BEGIN_ENV/END_ENV in the pod log equal WANT_ENV exactly.
expect_env() {
	local name=$1 requests=$2 config=$3 want=$4
	teardown_case "${name}"
	{ claim "${name}" "${requests}" "${config}"; echo "---"; pod "${name}"; } | kubectl apply -f - >/dev/null
	if ! kubectl -n "${E2E_NAMESPACE}" wait "pod/${name}" --for=condition=Ready --timeout="${POD_TIMEOUT}" >/dev/null 2>&1; then
		fail "${name}: pod did not become Ready"
		kubectl -n "${E2E_NAMESPACE}" describe pod "${name}" | tail -15
		teardown_case "${name}"
		return
	fi
	local got
	got="$(kubectl -n "${E2E_NAMESPACE}" logs "${name}" | sed -n '/^BEGIN_ENV$/,/^END_ENV$/p' | sed '1d;$d')"
	if [ "${got}" = "${want}" ]; then
		pass "${name}: env [$(echo "${got}" | tr '\n' ' ')]"
	else
		fail "${name}: env got [$(echo "${got}" | tr '\n' ' ')] want [$(echo "${want}" | tr '\n' ' ')]"
	fi
	teardown_case "${name}"
}

# expect_prepare_failure NAME REQUESTS CONFIG SUBSTRING
# Applies the claim and pod and asserts that the kubelet reports a
# FailedPrepareDynamicResources event containing SUBSTRING and that the pod
# never runs.
expect_prepare_failure() {
	local name=$1 requests=$2 config=$3 want=$4
	teardown_case "${name}"
	{ claim "${name}" "${requests}" "${config}"; echo "---"; pod "${name}"; } | kubectl apply -f - >/dev/null
	local msg="" phase=""
	for _ in $(seq 1 "${PREPARE_FAILURE_TIMEOUT}"); do
		msg="$(kubectl -n "${E2E_NAMESPACE}" get events --field-selector "involvedObject.name=${name},reason=FailedPrepareDynamicResources" -o jsonpath='{.items[*].message}' 2>/dev/null || true)"
		[ -n "${msg}" ] && break
		sleep 1
	done
	phase="$(kubectl -n "${E2E_NAMESPACE}" get pod "${name}" -o jsonpath='{.status.phase}')"
	if [[ "${msg}" == *"${want}"* ]] && [ "${phase}" = "Pending" ]; then
		pass "${name}: prepare rejected (${want})"
	else
		fail "${name}: phase=${phase} events=[${msg}] want substring [${want}]"
	fi
	teardown_case "${name}"
}

# ---------------------------------------------------------------------------
# Test cases
# ---------------------------------------------------------------------------

log "Creating DeviceClass with a class-level config"
kubectl apply -f - >/dev/null <<MANIFEST
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata: {name: tpu-e2e-class-config}
spec:
  selectors:
  - cel: {expression: 'device.driver == "${DRIVER_NAME}"'}
  config:
  - $(tpu_config 1)
MANIFEST

expect_env no-config "${ALL_TPUS}" "" ""

expect_env class-config "${ALL_TPUS_CLASS_CONFIG}" "" \
	"$(printf 'TPU_MIN_LOG_LEVEL=1\nTPU_STDERR_LOG_LEVEL=1')"

expect_env claim-config "${ALL_TPUS}" "    - $(tpu_config 2)" \
	"$(printf 'TPU_MIN_LOG_LEVEL=2\nTPU_STDERR_LOG_LEVEL=2')"

expect_env claim-overrides-class "${ALL_TPUS_CLASS_CONFIG}" "    - $(tpu_config 3)" \
	"$(printf 'TPU_MIN_LOG_LEVEL=3\nTPU_STDERR_LOG_LEVEL=3')"

expect_env later-claim-config-wins "${ALL_TPUS}" \
	"$(printf '    - %s\n    - %s' "$(tpu_config 1)" "$(tpu_config 2)")" \
	"$(printf 'TPU_MIN_LOG_LEVEL=2\nTPU_STDERR_LOG_LEVEL=2')"

expect_env request-scoped-config "${ALL_TPUS}" \
	"    - requests: [tpus]
      $(tpu_config 0 3)" \
	"$(printf 'TPU_MIN_LOG_LEVEL=0\nTPU_STDERR_LOG_LEVEL=3')"

expect_env other-driver-config-ignored "${ALL_TPUS}" \
	"    - opaque: {driver: other.example.com, parameters: {apiVersion: other.example.com/v1, kind: Whatever, bogus: true}}" \
	""

expect_env main-request-config-applies-to-subrequest "${FIRST_AVAILABLE}" \
	"    - requests: [tpus]
      $(tpu_config 2)" \
	"$(printf 'TPU_MIN_LOG_LEVEL=2\nTPU_STDERR_LOG_LEVEL=2')"

expect_env exact-subrequest-config "${FIRST_AVAILABLE}" \
	"    - requests: [tpus/eight]
      $(tpu_config 3)
    - requests: [tpus/four]
      $(tpu_config 1)" \
	"$(printf 'TPU_MIN_LOG_LEVEL=1\nTPU_STDERR_LOG_LEVEL=1')"

expect_env agreeing-configs-across-requests "${SPLIT_REQUESTS}" \
	"    - requests: [a]
      $(tpu_config 2)
    - requests: [b]
      $(tpu_config 2)" \
	"$(printf 'TPU_MIN_LOG_LEVEL=2\nTPU_STDERR_LOG_LEVEL=2')"

expect_prepare_failure conflicting-configs-across-requests "${SPLIT_REQUESTS}" \
	"    - requests: [a]
      $(tpu_config 1)
    - requests: [b]
      $(tpu_config 2)" \
	"conflicting TPU configs within the claim"

expect_prepare_failure unknown-field "${ALL_TPUS}" \
	"    - opaque: {driver: ${DRIVER_NAME}, parameters: {apiVersion: ${CONFIG_API_VERSION}, kind: TpuConfig, loggingg: {level: 1}}}" \
	'unknown field "loggingg"'

expect_prepare_failure unknown-kind "${ALL_TPUS}" \
	"    - opaque: {driver: ${DRIVER_NAME}, parameters: {apiVersion: ${CONFIG_API_VERSION}, kind: NotATpuConfig}}" \
	'no kind "NotATpuConfig" is registered'

expect_prepare_failure out-of-range-level "${ALL_TPUS}" "    - $(tpu_config 9)" \
	"invalid level: 9"

kubectl delete deviceclass tpu-e2e-class-config --ignore-not-found >/dev/null

log "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -ne 0 ]; then
	printf '[e2e]   - %s\n' "${FAILED_CASES[@]}"
	exit 1
fi
