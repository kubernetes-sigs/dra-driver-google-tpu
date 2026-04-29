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

# A reference to the current directory where this script is located
CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"

set -ex
set -o pipefail

source "${CURRENT_DIR}/common.sh"

# Variables REGISTRY, IMAGE, and TAG are inherited from environment or set as defaults in common.sh

echo "REGISTRY=${REGISTRY}"
echo "IMAGE=${IMAGE}"
echo "TAG=${TAG}"

# Go back to the root directory of this repo
cd "${CURRENT_DIR}/../.."

if [[ "${MULTI_ARCH:-}" == "true" ]]; then
  # Push multi-arch images and manifest
  # deployments/container/Makefile assumes IMAGE is just the name for these targets
  make -f deployments/container/Makefile push-all-individual build-multi-arch push-multi-arch \
    REGISTRY="${REGISTRY}" \
    IMAGE="${IMAGE}" \
    TAG="${TAG}"
else
  # Push single arch
  # deployments/container/Makefile docker-push target assumes IMAGE is the full tag
  make -f deployments/container/Makefile docker-push \
    IMAGE="${REGISTRY}/${IMAGE}:${TAG}"
fi
