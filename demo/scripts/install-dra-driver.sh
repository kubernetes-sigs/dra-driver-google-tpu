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


CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"
PROJECT_DIR="$(cd -- "$( dirname -- "${CURRENT_DIR}/../../../" )" &> /dev/null && pwd)"

set -o pipefail

source "${CURRENT_DIR}/common.sh"

HELM="${HELM:-helm}"

# Optionally map the DeviceClass to a classic extended resource name
# (KEP-5004), e.g. EXTENDED_RESOURCE_NAME=google.com/tpu.
EXTRA_HELM_ARGS=()
if [[ -n "${EXTENDED_RESOURCE_NAME:-}" ]]; then
  EXTRA_HELM_ARGS+=(--set "deviceClass.extendedResourceName=${EXTENDED_RESOURCE_NAME}")
fi

${HELM} upgrade -i --create-namespace --namespace dra-driver-google-tpu dra-driver-google-tpu ${PROJECT_DIR}/deployments/helm/dra-driver-google-tpu \
  --set image.repository=${REGISTRY}/${IMAGE} \
  --set image.tag=${TAG} \
  --set image.pullPolicy=IfNotPresent \
  --set kubeletPlugin.priorityClassName="" \
  --set kubeletPlugin.tolerations[0].key=google.com/tpu \
  --set kubeletPlugin.tolerations[0].operator=Exists \
  --set kubeletPlugin.tolerations[0].effect=NoSchedule \
  "${EXTRA_HELM_ARGS[@]}"