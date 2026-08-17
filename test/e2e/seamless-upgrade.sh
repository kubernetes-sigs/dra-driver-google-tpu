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

# End-to-end test for seamless (rolling) upgrades of the kubelet plugin.
#
# The plugin passes its pod UID to kubeletplugin.RollingUpdate so each
# DaemonSet pod registers on UID-suffixed sockets, reports readiness only once
# the kubelet has confirmed its registration, and the chart rolls the
# DaemonSet with maxSurge=1 / maxUnavailable=0. This test creates a kind
# cluster, fakes a 4-chip tpu-v4-podslice node the same way the kind demo does
# (node labels plus /dev/accel* device nodes), builds the driver from the
# working tree, and drives the upgrade paths a cluster admin would take:
#
#   1. Baseline: install the driver built from BASELINE_REF (fixed socket
#      names, no surge, no readiness probe) and run a workload holding a claim
#      on all TPUs.
#   2. Upgrade baseline -> working tree: the incoming pod registers on suffixed
#      sockets while the outgoing pod is still registered, the DaemonSet
#      controller removes the outgoing pod only after the kubelet registered
#      the incoming one, the workload keeps running with no restarts and no
#      Prepare/Unprepare traffic, and the old sockets are gone once the old
#      pod exits.
#   3. Upgrade working tree -> working tree (a new image tag): both instances
#      now use suffixed sockets; same assertions. Afterwards the new instance
#      unprepares the claim that the baseline instance prepared (shared
#      checkpoint) and prepares a fresh one.
#   4. Upgrade to an image that cannot be pulled, then helm rollback: the
#      running instance is never touched (same pod UID before and after).
#   5. Upgrade to images that start but never register — one whose binary
#      exits immediately, one that runs a while and then dies, and a real
#      driver pointed at a registry directory the kubelet does not watch —
#      then helm rollback each time: the readiness probe keeps the surge pod
#      unavailable, so the healthy pod is never removed.
#   6. Kill -9 the plugin process: the restarted container in the same pod
#      re-registers on the same sockets and becomes ready again.
#
# During every rollout an on-node sampler records the registration / DRA
# sockets every 50ms, a pod watch records pod state transitions, a
# ResourceSlice watch counts slice writes, and the kubelet journal is checked
# for its plugin (de)registration records, so overlap, ordering, socket
# cleanup, driverless gaps and slice churn are measured rather than assumed.
#
# The GKE-only init/sidecar containers of the DaemonSet are removed by a helm
# post-renderer because they need real GKE infrastructure.
#
# Usage: make test-e2e-seamless-upgrade   (or run this script directly)
# Knobs: K8S_VERSION, KIND_IMAGE, KIND_CLUSTER_NAME, BASELINE_REF (git ref to
#        build the "before" driver from; empty skips the baseline install and
#        the baseline -> working tree upgrade), KEEP_CLUSTER=true, KIND, HELM,
#        KUBECTL, YQ. HELM must be Helm 3: the exec post-renderer this test
#        uses to strip the GKE-only containers was replaced by plugins in
#        Helm 4, so the default runs Helm 3 through `go run` like the Makefile.

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"
PROJECT_DIR="$(cd -- "${CURRENT_DIR}/../.." &> /dev/null && pwd)"

set -eu
set -o pipefail

: ${KIND:="kind"}
: ${HELM:="go run helm.sh/helm/v3/cmd/helm@v3.20.1"}
: ${KUBECTL:="kubectl"}
: ${YQ:="yq"}
: ${K8S_VERSION:="v1.35.0"}
: ${KIND_CLUSTER_NAME:="tpu-seamless-upgrades-e2e"}
: ${KIND_IMAGE:="kindest/node:${K8S_VERSION}"}
: ${BASELINE_REF:="origin/main"}
: ${KEEP_CLUSTER:="false"}
: ${IMAGE_REPO:="tpu-dra-driver"}
: ${IMAGE_TAG_PREFIX:="seamless-e2e"}

WORKER_NODE="${KIND_CLUSTER_NAME}-worker"
HELM_RELEASE="tpu-dra-e2e"
HELM_NAMESPACE="dra-driver-google-tpu"
TEST_NAMESPACE="tpu-test"
WORKLOAD_POD="tpu-pod0"
DRIVER_NAME="tpu.google.com"
REGISTRY_DIR="/var/lib/kubelet/plugins_registry"
PLUGIN_DIR="/var/lib/kubelet/plugins/${DRIVER_NAME}"
POD_SELECTOR="app.kubernetes.io/name=dra-driver-google-tpu"

TAG_MAIN="${IMAGE_TAG_PREFIX}-main"
TAG_A="${IMAGE_TAG_PREFIX}-a"
TAG_B="${IMAGE_TAG_PREFIX}-b"
TAG_CRASH="${IMAGE_TAG_PREFIX}-crash"
TAG_SLOWCRASH="${IMAGE_TAG_PREFIX}-slowcrash"
TAG_MISSING="${IMAGE_TAG_PREFIX}-does-not-exist"

WORK_DIR="$(mktemp -d)"
# Never touch the user's ~/.kube/config: kind writes the cluster's kubeconfig
# here and every kubectl/helm call below uses it (other kind clusters may be
# created concurrently on the same machine).
export KUBECONFIG="${WORK_DIR}/kubeconfig"
FAILURES=0
MONITOR_LOG=""; SLICE_LOG=""; PODS_LOG=""
SLICE_WATCH_PID=""; PODS_WATCH_PID=""; LOG_PIDS=()

log() { printf '\n=== %s ===\n' "$*"; }
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILURES=$((FAILURES + 1)); }
# check <description> <shell expression>: PASS if the expression succeeds.
check() {
    local desc="$1" expr="$2"
    if eval "${expr}"; then pass "${desc}"; else fail "${desc}"; fi
}

cleanup() {
    stop_monitors || true
    if [[ "${KEEP_CLUSTER}" == "true" ]]; then
        echo "KEEP_CLUSTER=true: leaving kind cluster ${KIND_CLUSTER_NAME} running; artifacts in ${WORK_DIR}"
    else
        ${KIND} delete cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" || true
        rm -rf "${WORK_DIR}"
    fi
}
trap cleanup EXIT

worker() { docker exec "${WORKER_NODE}" "$@"; }
node_now() { worker date -u '+%Y-%m-%d %H:%M:%S'; }

# Sockets as the kubelet sees them on the worker's filesystem.
reg_sockets() { worker sh -c "ls ${REGISTRY_DIR} 2>/dev/null | grep '^${DRIVER_NAME}' | sort | tr '\n' ' '"; }
dra_sockets() { worker sh -c "ls ${PLUGIN_DIR} 2>/dev/null | grep '\.sock$' | sort | tr '\n' ' '"; }

# name uid phase ready image, one line per plugin pod
plugin_pods() {
    ${KUBECTL} get pods -n "${HELM_NAMESPACE}" -l "${POD_SELECTOR}" -o json \
        | jq -r '.items[] | [.metadata.name, .metadata.uid, .status.phase,
            ((.status.conditions // []) | map(select(.type=="Ready")) | .[0].status // "Unknown"),
            (.spec.containers[] | select(.name=="tpu-dra-plugin") | .image)] | @tsv'
}
plugin_pod_count() { plugin_pods | wc -l | tr -d ' '; }
ready_plugin_pod_name() { plugin_pods | awk '$3=="Running" && $4=="True"{print $1}' | head -1; }
ready_plugin_pod_uid()  { plugin_pods | awk '$3=="Running" && $4=="True"{print $2}' | head -1; }
plugin_pod_restarts()   { ${KUBECTL} get pod -n "${HELM_NAMESPACE}" "$1" -o jsonpath='{.status.containerStatuses[0].restartCount}'; }
wait_for_plugin_pod_count() { # wait_for_plugin_pod_count <n> <seconds>
    for _ in $(seq 1 "$2"); do [[ "$(plugin_pod_count)" == "$1" ]] && return 0; sleep 1; done
    return 1
}

workload_container_id() { ${KUBECTL} get pod -n "${TEST_NAMESPACE}" "${WORKLOAD_POD}" -o jsonpath='{.status.containerStatuses[0].containerID}'; }
workload_restarts()     { ${KUBECTL} get pod -n "${TEST_NAMESPACE}" "${WORKLOAD_POD}" -o jsonpath='{.status.containerStatuses[0].restartCount}'; }
workload_phase()        { ${KUBECTL} get pod -n "${TEST_NAMESPACE}" "${WORKLOAD_POD}" -o jsonpath='{.status.phase}'; }
slice_device_count()    { ${KUBECTL} get resourceslices -o json | jq "[.items[] | select(.spec.driver==\"${DRIVER_NAME}\") | .spec.devices[]] | length"; }
checkpoint_claims()     { worker cat "${PLUGIN_DIR}/checkpoint.json" | jq '.v1.preparedClaims | length'; }

CHART_DIR="${PROJECT_DIR}/deployments/helm/dra-driver-google-tpu"
BASELINE_CHART_DIR="${WORK_DIR}/baseline/deployments/helm/dra-driver-google-tpu"

install_driver() { # install_driver <chart dir> <image tag> [extra helm args...]
    local chart="$1" tag="$2"; shift 2
    ${HELM} upgrade -i --create-namespace --namespace "${HELM_NAMESPACE}" "${HELM_RELEASE}" \
        "${chart}" \
        --post-renderer "${CURRENT_DIR}/strip-gke-containers.sh" \
        --set image.repository="docker.io/library/${IMAGE_REPO}" \
        --set image.tag="${tag}" \
        --set image.pullPolicy=Never \
        --set kubeletPlugin.priorityClassName="" \
        "$@" > /dev/null
    echo "helm: ${HELM_RELEASE} now at revision $(current_revision) (chart ${chart#${WORK_DIR}/}, image ${IMAGE_REPO}:${tag} $*)"
}
current_revision() { ${HELM} history -n "${HELM_NAMESPACE}" "${HELM_RELEASE}" -o json | jq '.[-1].revision'; }
rollback_to() { # rollback_to <revision>
    ${HELM} rollback -n "${HELM_NAMESPACE}" "${HELM_RELEASE}" "$1" --wait --timeout 300s > /dev/null
    echo "helm: rolled back to revision $1 (now revision $(current_revision))"
}
wait_rollout() { ${KUBECTL} rollout status -n "${HELM_NAMESPACE}" daemonset --timeout=300s; }
wait_for_slice() {
    for _ in $(seq 1 60); do [[ "$(slice_device_count)" == "4" ]] && return 0; sleep 2; done
    echo "ResourceSlice for ${DRIVER_NAME} never reached 4 devices"; return 1
}

# --- rollout monitors -----------------------------------------------------
# On-node sampler: every 50ms, "epoch-ms|reg sockets|dra sockets". Runs inside
# the worker so the two directory listings are taken back to back.
start_monitors() { # start_monitors <stage-name>
    local stage="$1"
    MONITOR_LOG="${WORK_DIR}/${stage}.sockets"
    SLICE_LOG="${WORK_DIR}/${stage}.slices"
    PODS_LOG="${WORK_DIR}/${stage}.pods"
    : > "${SLICE_LOG}"; : > "${PODS_LOG}"
    worker sh -c "rm -f /tmp/e2e-sockets.log; nohup sh -c 'while true; do echo \"\$(date +%s%3N)|\$(ls ${REGISTRY_DIR} 2>/dev/null | grep ^${DRIVER_NAME} | sort | tr \"\\n\" \" \")|\$(ls ${PLUGIN_DIR} 2>/dev/null | grep sock\$ | sort | tr \"\\n\" \" \")\"; sleep 0.05; done' > /tmp/e2e-sockets.log 2>/dev/null & echo \$! > /tmp/e2e-sockets.pid"
    # Every write to a ResourceSlice of ours produces one line.
    ${KUBECTL} get resourceslices --watch-only -o json 2>/dev/null \
        | jq --unbuffered -r "select(.spec.driver==\"${DRIVER_NAME}\") | [.metadata.name, .metadata.resourceVersion, (.spec.devices|length)] | @tsv" \
        >> "${SLICE_LOG}" &
    SLICE_WATCH_PID=$!
    # Every plugin pod state change: name uid phase ready readySince deletionTimestamp deletionGracePeriodSeconds
    ${KUBECTL} get pods -n "${HELM_NAMESPACE}" -l "${POD_SELECTOR}" --watch-only -o json 2>/dev/null \
        | jq --unbuffered -r '[.metadata.name, .metadata.uid, .status.phase,
            ((.status.conditions // []) | map(select(.type=="Ready")) | .[0].status // "Unknown"),
            ((.status.conditions // []) | map(select(.type=="Ready")) | .[0].lastTransitionTime // "-"),
            (.metadata.deletionTimestamp // "-"), (.metadata.deletionGracePeriodSeconds // 0)] | @tsv' >> "${PODS_LOG}" &
    PODS_WATCH_PID=$!
    # Stream new log lines of every plugin pod that exists now; pods that get
    # deleted take their logs with them, so this is the only record of what
    # the outgoing instance did during the rollout.
    for p in $(plugin_pods | awk '{print $1}'); do
        ${KUBECTL} logs -n "${HELM_NAMESPACE}" "${p}" -c tpu-dra-plugin -f --tail=0 > "${WORK_DIR}/${stage}.${p}.log" 2>/dev/null &
        LOG_PIDS+=($!)
    done
    sleep 1
}
stop_monitors() {
    if [[ -n "${MONITOR_LOG}" ]]; then
        worker sh -c 'kill $(cat /tmp/e2e-sockets.pid) 2>/dev/null; cat /tmp/e2e-sockets.log' > "${MONITOR_LOG}" 2>/dev/null || true
    fi
    for pid in "${SLICE_WATCH_PID}" "${PODS_WATCH_PID}" "${LOG_PIDS[@]:-}"; do
        [[ -n "${pid}" ]] || continue
        pkill -P "${pid}" 2>/dev/null || true; kill "${pid}" 2>/dev/null || true
    done
    SLICE_WATCH_PID=""; PODS_WATCH_PID=""; LOG_PIDS=()
    return 0
}

# Socket sampler analysis: whether two registration / DRA sockets were ever
# present at once, and the longest run of samples with no registration socket
# at all (a driverless window).
analyze_sockets() { # analyze_sockets <sockets log>
    python3 - "$1" <<'PY'
import sys
rows=[l.rstrip("\n").split("|") for l in open(sys.argv[1]) if l.count("|")==2]
dual_reg=dual_dra=False; gap_ms=0; gap_start=None; prev=None
for ts,regs,dras in rows:
    ts=int(ts)
    if len(regs.split())>=2: dual_reg=True
    if len(dras.split())>=2: dual_dra=True
    if not regs.split():
        if gap_start is None: gap_start=prev if prev else ts
    elif gap_start is not None:
        gap_ms=max(gap_ms, ts-gap_start); gap_start=None
    prev=ts
if gap_start is not None and prev is not None: gap_ms=max(gap_ms, prev-gap_start)
print(f"samples={len(rows)} dual_reg_sockets={dual_reg} dual_dra_sockets={dual_dra} max_no_reg_socket_ms={gap_ms}")
PY
}

# Pod watch analysis for a rollout from <old uid> to <new uid>: the time the
# new pod became Ready and the time the old pod's deletion was requested
# (deletionTimestamp minus the grace period, which is how the API records it).
# Prints "new_ready=<ts> old_deletion_requested=<ts> ordered=<bool>".
analyze_pods() { # analyze_pods <pods log> <old uid> <new uid>
    python3 - "$1" "$2" "$3" <<'PY'
import sys
from datetime import datetime, timedelta
rows=[l.rstrip("\n").split("\t") for l in open(sys.argv[1]) if l.strip()]
old,new=sys.argv[2],sys.argv[3]
def p(t): return datetime.strptime(t,"%Y-%m-%dT%H:%M:%SZ")
new_ready=old_del=None
for name,uid,phase,ready,ready_since,deleted,grace in rows:
    if uid==new and ready=="True" and ready_since!="-" and new_ready is None: new_ready=p(ready_since)
    if uid==old and deleted!="-" and old_del is None: old_del=p(deleted)-timedelta(seconds=int(grace))
ordered = new_ready is not None and old_del is not None and new_ready <= old_del
fmt=lambda t: t.strftime("%Y-%m-%dT%H:%M:%SZ") if t else None
print(f"new_ready={fmt(new_ready)} old_deletion_requested={fmt(old_del)} ordered={ordered}")
PY
}

kubelet_dra_log() { # kubelet_dra_log <since>: kubelet's DRA plugin (de)registration records
    worker journalctl -u kubelet --since "$1" --no-pager -o short-iso 2>/dev/null \
        | grep -E "Registered DRA plugin|Unregistered DRA plugin" || true
}

# Runs a helm upgrade to <tag> under the monitors and asserts the seamless
# properties. <mode> is "fixed-to-uid" (outgoing pod uses fixed socket names)
# or "uid-to-uid".
seamless_upgrade() { # seamless_upgrade <stage> <tag> <mode>
    local stage="$1" tag="$2" mode="$3"
    local old_pod old_uid old_cid old_restarts since
    old_pod=$(ready_plugin_pod_name); old_uid=$(ready_plugin_pod_uid)
    old_cid=$(workload_container_id); old_restarts=$(workload_restarts)
    since=$(node_now)
    log "${stage}: upgrade to ${IMAGE_REPO}:${tag} (outgoing pod ${old_pod} uid ${old_uid})"
    echo "sockets before: reg=[$(reg_sockets)] dra=[$(dra_sockets)]"

    start_monitors "${stage}"
    install_driver "${CHART_DIR}" "${tag}"
    wait_rollout
    # Let the kubelet finish deregistering and the outgoing pod's sockets disappear.
    sleep 5
    stop_monitors

    local new_pod new_uid sockets pods kl
    new_pod=$(ready_plugin_pod_name); new_uid=$(ready_plugin_pod_uid)
    sockets=$(analyze_sockets "${MONITOR_LOG}")
    pods=$(analyze_pods "${PODS_LOG}" "${old_uid}" "${new_uid}")
    kl=$(kubelet_dra_log "${since}")
    ${KUBECTL} logs -n "${HELM_NAMESPACE}" "${new_pod}" -c tpu-dra-plugin > "${WORK_DIR}/${stage}.${new_pod}.new.log" 2>/dev/null || true
    echo "socket sampler: ${sockets}"
    echo "pod watch: ${pods}"
    echo "sockets after: reg=[$(reg_sockets)] dra=[$(dra_sockets)]"
    echo "plugin pods after:"; plugin_pods
    echo "ResourceSlice writes during rollout: $(wc -l < "${SLICE_LOG}" | tr -d ' ')"; cat "${SLICE_LOG}"
    echo "kubelet DRA plugin registration records since ${since}:"; echo "${kl}" | sed 's/^/  /'
    echo "socket samples around the handover:"
    grep -n " .*[a-z0-9] \|^[0-9]*||" "${MONITOR_LOG}" | head -3 | sed 's/^/  /'; grep -c "" "${MONITOR_LOG}" | sed 's/^/  total samples: /'

    check "${stage}: exactly one plugin pod remains" "[ \"$(plugin_pod_count)\" = 1 ]"
    check "${stage}: the surviving pod is a new pod" "[ \"${new_uid}\" != \"${old_uid}\" ] && [ -n \"${new_uid}\" ]"
    check "${stage}: surviving pod runs the new image" "[ \"$(plugin_pods | awk '{print $5}')\" = docker.io/library/${IMAGE_REPO}:${tag} ]"
    check "${stage}: registration socket is UID-suffixed for the new pod" "[ \"$(reg_sockets)\" = '${DRIVER_NAME}-${new_uid}-reg.sock ' ]"
    check "${stage}: DRA socket is UID-suffixed for the new pod" "[ \"$(dra_sockets)\" = 'dra-${new_uid}.sock ' ]"
    check "${stage}: both registration sockets coexisted (dual registration)" "[[ '${sockets}' == *'dual_reg_sockets=True'* ]]"
    check "${stage}: both DRA sockets coexisted" "[[ '${sockets}' == *'dual_dra_sockets=True'* ]]"
    check "${stage}: no 50ms sample without a registration socket (no driverless window)" "[[ '${sockets}' == *'max_no_reg_socket_ms=0'* ]]"
    check "${stage}: new pod was Ready before the old pod was deleted (surge ordering)" "[[ '${pods}' == *'ordered=True'* ]]"
    check "${stage}: kubelet registered the new instance" "grep -q 'Registered DRA plugin.*dra-${new_uid}.sock' <<<'${kl}'"
    check "${stage}: kubelet had both instances registered at once (numPlugins=2)" "grep -q 'Registered DRA plugin.*numPlugins=2' <<<'${kl}'"
    if [[ "${mode}" == "fixed-to-uid" ]]; then
        check "${stage}: kubelet unregistered the outgoing instance" "grep -q 'Unregistered DRA plugin.*${PLUGIN_DIR}/dra.sock' <<<'${kl}'"
        check "${stage}: fixed-name sockets are gone" "! worker sh -c 'test -e ${REGISTRY_DIR}/${DRIVER_NAME}-reg.sock -o -e ${PLUGIN_DIR}/dra.sock'"
    else
        check "${stage}: kubelet unregistered the outgoing instance" "grep -q 'Unregistered DRA plugin.*dra-${old_uid}.sock' <<<'${kl}'"
        check "${stage}: outgoing pod's sockets are gone" "! worker sh -c 'test -e ${REGISTRY_DIR}/${DRIVER_NAME}-${old_uid}-reg.sock -o -e ${PLUGIN_DIR}/dra-${old_uid}.sock'"
    fi
    check "${stage}: workload pod still Running" "[ \"$(workload_phase)\" = Running ]"
    check "${stage}: workload container not restarted" "[ \"$(workload_container_id)\" = \"${old_cid}\" ] && [ \"$(workload_restarts)\" = \"${old_restarts}\" ]"
    check "${stage}: ResourceSlice still publishes 4 devices" "[ \"$(slice_device_count)\" = 4 ]"
    # The kubelet only asks a plugin to (un)prepare on pod create/delete; a
    # driver rollout must not generate any such traffic for a running workload.
    local calls
    calls=$(cat "${WORK_DIR}/${stage}."*.log 2>/dev/null | grep -c "PrepareResource is called\|UnPrepareResource is called" || true)
    check "${stage}: no Prepare/Unprepare calls during rollout (saw ${calls})" "[ \"${calls}\" = 0 ]"
    check "${stage}: at most 2 ResourceSlice writes during rollout" "[ \"$(wc -l < "${SLICE_LOG}" | tr -d ' ')\" -le 2 ]"
}

# Upgrades to an image / configuration that never registers with the kubelet
# and asserts the healthy pod is left alone, then rolls back.
failed_upgrade() { # failed_upgrade <stage> <description> <tag> [extra helm args...]
    local stage="$1" desc="$2" tag="$3"; shift 3
    local good_uid good_pod cid rev since
    good_uid=$(ready_plugin_pod_uid); good_pod=$(ready_plugin_pod_name); cid=$(workload_container_id)
    rev=$(current_revision); since=$(node_now)
    log "${stage}: ${desc}, then roll back (healthy pod ${good_pod} uid ${good_uid})"
    start_monitors "${stage}"
    install_driver "${CHART_DIR}" "${tag}" "$@"
    # Give the DaemonSet controller ample time to (wrongly) replace the healthy pod.
    sleep 45
    stop_monitors
    plugin_pods
    echo "socket sampler: $(analyze_sockets "${MONITOR_LOG}")"
    check "${stage}: healthy pod untouched while the surge pod never becomes ready" "[ \"$(ready_plugin_pod_uid)\" = \"${good_uid}\" ]"
    check "${stage}: surge pod exists but is not Ready" "plugin_pods | grep -v \"${good_pod}\" | grep -q . && ! plugin_pods | grep -v \"${good_pod}\" | awk '{print \$4}' | grep -q True"
    check "${stage}: no 50ms sample without a registration socket" "[[ '$(analyze_sockets "${MONITOR_LOG}")' == *'max_no_reg_socket_ms=0'* ]]"
    check "${stage}: healthy pod still registered" "[ \"$(reg_sockets)\" = '${DRIVER_NAME}-${good_uid}-reg.sock ' ]"
    check "${stage}: workload unaffected" "[ \"$(workload_container_id)\" = \"${cid}\" ]"
    rollback_to "${rev}"
    wait_rollout
    check "${stage}: rollback removed the surge pod" "wait_for_plugin_pod_count 1 90"
    plugin_pods
    check "${stage}: rollback kept the same healthy pod (no restart)" "[ \"$(ready_plugin_pod_uid)\" = \"${good_uid}\" ]"
    check "${stage}: registration unchanged" "[ \"$(reg_sockets)\" = '${DRIVER_NAME}-${good_uid}-reg.sock ' ]"
    check "${stage}: kubelet never unregistered the healthy instance" "! kubelet_dra_log '${since}' | grep -q 'Unregistered DRA plugin.*dra-${good_uid}.sock'"
}

# --- cluster ---------------------------------------------------------------
log "creating kind cluster ${KIND_CLUSTER_NAME} (${KIND_IMAGE})"
${KIND} create cluster \
    --name "${KIND_CLUSTER_NAME}" \
    --image "${KIND_IMAGE}" \
    --config "${CURRENT_DIR}/kind-cluster-config.yaml" \
    --kubeconfig "${KUBECONFIG}" \
    --wait 2m
${KUBECTL} get nodes -o wide

# Fake the TPU devices of a 4-chip v4 node (the driver discovers
# /dev/accel[0-9]* for tpu-v4-podslice).
for i in 0 1 2 3; do
    worker bash -c "mknod -m 666 /dev/accel${i} b 100 ${i} 2>/dev/null || true"
done

# --- images ----------------------------------------------------------------
log "building driver images"
docker build -q -f "${PROJECT_DIR}/deployments/container/Dockerfile" -t "${IMAGE_REPO}:${TAG_A}" "${PROJECT_DIR}"
docker tag "${IMAGE_REPO}:${TAG_A}" "${IMAGE_REPO}:${TAG_B}"
if [[ -n "${BASELINE_REF}" ]]; then
    # The baseline is installed exactly as it shipped: its own chart and image.
    git -C "${PROJECT_DIR}" archive --format=tar "${BASELINE_REF}" > "${WORK_DIR}/baseline.tar"
    mkdir -p "${WORK_DIR}/baseline" && tar -xf "${WORK_DIR}/baseline.tar" -C "${WORK_DIR}/baseline"
    docker build -q -f deployments/container/Dockerfile -t "${IMAGE_REPO}:${TAG_MAIN}" - < "${WORK_DIR}/baseline.tar"
fi
# "Broken builds": the plugin binary exits immediately / runs a while and dies.
docker build -q -t "${IMAGE_REPO}:${TAG_CRASH}" - <<'DOCKERFILE'
FROM busybox:1.36
RUN printf '#!/bin/sh\necho "simulated broken driver build"; exit 1\n' > /usr/bin/tpu-dra-kubeletplugin && chmod +x /usr/bin/tpu-dra-kubeletplugin
DOCKERFILE
docker build -q -t "${IMAGE_REPO}:${TAG_SLOWCRASH}" - <<'DOCKERFILE'
FROM busybox:1.36
RUN printf '#!/bin/sh\necho "simulated driver that starts, then dies"; sleep 8; exit 1\n' > /usr/bin/tpu-dra-kubeletplugin && chmod +x /usr/bin/tpu-dra-kubeletplugin
DOCKERFILE
for t in "${TAG_A}" "${TAG_B}" "${TAG_CRASH}" "${TAG_SLOWCRASH}" ${BASELINE_REF:+"${TAG_MAIN}"}; do
    ${KIND} load docker-image "${IMAGE_REPO}:${t}" --name "${KIND_CLUSTER_NAME}" > /dev/null
done
docker images "${IMAGE_REPO}" --format '{{.Repository}}:{{.Tag}} {{.ID}}' | grep "${IMAGE_TAG_PREFIX}"

# --- 1. baseline -----------------------------------------------------------
if [[ -n "${BASELINE_REF}" ]]; then
    log "stage 1: install baseline chart and driver from ${BASELINE_REF} ($(git -C "${PROJECT_DIR}" rev-parse --short "${BASELINE_REF}"))"
    install_driver "${BASELINE_CHART_DIR}" "${TAG_MAIN}"
else
    log "stage 1: install driver from the working tree"
    install_driver "${CHART_DIR}" "${TAG_A}"
fi
wait_rollout
wait_for_slice
plugin_pods
echo "sockets: reg=[$(reg_sockets)] dra=[$(dra_sockets)]"
if [[ -n "${BASELINE_REF}" ]]; then
    check "stage 1: baseline uses fixed socket names" "[ \"$(reg_sockets)\" = '${DRIVER_NAME}-reg.sock ' ] && [ \"$(dra_sockets)\" = 'dra.sock ' ]"
fi

log "stage 1: run a workload claiming all TPUs"
${KUBECTL} apply -f "${PROJECT_DIR}/demo/specs/tpu-test.yaml"
${KUBECTL} wait -n "${TEST_NAMESPACE}" "pod/${WORKLOAD_POD}" --for=condition=Ready --timeout=180s
echo "workload container: $(workload_container_id)"
check "stage 1: claim recorded in the shared checkpoint" "[ \"$(checkpoint_claims)\" = 1 ]"

# --- 2. baseline -> working tree -------------------------------------------
if [[ -n "${BASELINE_REF}" ]]; then
    seamless_upgrade "stage2" "${TAG_A}" "fixed-to-uid"
fi

# --- 3. working tree -> working tree ---------------------------------------
seamless_upgrade "stage3" "${TAG_B}" "uid-to-uid"

log "stage 3: the new instance unprepares a claim prepared by an earlier instance, then prepares a fresh one"
${KUBECTL} delete pod -n "${TEST_NAMESPACE}" "${WORKLOAD_POD}" --timeout=120s
sleep 3
check "stage 3: checkpoint has no prepared claims after unprepare" "[ \"$(checkpoint_claims)\" = 0 ]"
check "stage 3: current instance handled the unprepare" "${KUBECTL} logs -n ${HELM_NAMESPACE} $(ready_plugin_pod_name) -c tpu-dra-plugin | grep -q 'UnPrepareResource is called'"
${KUBECTL} apply -f "${PROJECT_DIR}/demo/specs/tpu-test.yaml"
check "stage 3: a fresh workload becomes Ready" "${KUBECTL} wait -n ${TEST_NAMESPACE} pod/${WORKLOAD_POD} --for=condition=Ready --timeout=180s"
check "stage 3: fresh claim recorded in checkpoint" "[ \"$(checkpoint_claims)\" = 1 ]"

# --- 4. bad image (unpullable) + rollback ----------------------------------
failed_upgrade "stage4" "upgrade to an image that cannot be pulled" "${TAG_MISSING}"

# --- 5. images / configurations that never register + rollback -------------
failed_upgrade "stage5a" "upgrade to an image whose plugin exits immediately" "${TAG_CRASH}"
failed_upgrade "stage5b" "upgrade to an image whose plugin runs for a while and then dies" "${TAG_SLOWCRASH}"
failed_upgrade "stage5c" "upgrade to a driver registering in a directory the kubelet does not watch" "${TAG_B}" \
    --set kubeletPlugin.kubeletRegistrarDirectoryPath="${REGISTRY_DIR}-not-watched"
worker rm -rf "${REGISTRY_DIR}-not-watched"

# --- 6. plugin process killed -9 -------------------------------------------
log "stage 6: kill -9 the plugin process; the restarted container re-registers on the same sockets"
uid=$(ready_plugin_pod_uid); pod=$(ready_plugin_pod_name); cid=$(workload_container_id); since=$(node_now)
worker pkill -9 -f '^tpu-dra-kubeletplugin$'
for _ in $(seq 1 60); do [[ "$(plugin_pod_restarts "${pod}")" == "1" ]] && break; sleep 2; done
${KUBECTL} wait -n "${HELM_NAMESPACE}" --for=condition=Ready "pod/${pod}" --timeout=120s
sleep 3
plugin_pods
kl=$(kubelet_dra_log "${since}"); echo "${kl}" | sed 's/^/  /'
check "stage 6: same pod, container restarted once" "[ \"$(ready_plugin_pod_uid)\" = \"${uid}\" ] && [ \"$(plugin_pod_restarts "${pod}")\" = 1 ]"
check "stage 6: re-registered on the same UID-suffixed sockets" "[ \"$(reg_sockets)\" = '${DRIVER_NAME}-${uid}-reg.sock ' ] && [ \"$(dra_sockets)\" = 'dra-${uid}.sock ' ]"
check "stage 6: kubelet re-registered the plugin" "grep -q 'Registered DRA plugin.*dra-${uid}.sock' <<<'${kl}'"
check "stage 6: workload unaffected" "[ \"$(workload_container_id)\" = \"${cid}\" ]"
${KUBECTL} delete pod -n "${TEST_NAMESPACE}" "${WORKLOAD_POD}" --timeout=120s
${KUBECTL} apply -f "${PROJECT_DIR}/demo/specs/tpu-test.yaml"
check "stage 6: workload can be re-prepared after the restart" "${KUBECTL} wait -n ${TEST_NAMESPACE} pod/${WORKLOAD_POD} --for=condition=Ready --timeout=180s"

log "summary"
if [[ "${FAILURES}" -ne 0 ]]; then
    echo "seamless-upgrade e2e: ${FAILURES} check(s) FAILED"
    exit 1
fi
echo "seamless-upgrade e2e: all checks passed"
