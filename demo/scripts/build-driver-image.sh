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

# This scripts invokes `kind build image` so that the resulting
# image has a containerd with CDI support.
#
# Usage: kind-build-image.sh <tag of generated image>

# A reference to the current directory where this script is located
CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"

set -ex
set -o pipefail

source "${CURRENT_DIR}/common.sh"

# Create a temorary directory to hold all the artifacts we need for building the image
TMP_DIR="$(mktemp -d)"
cleanup() {
    rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

# Go back to the root directory of this repo
cd "${CURRENT_DIR}/../.."

# Variables REGISTRY, IMAGE, and TAG are inherited from environment or set as defaults in common.sh

echo "REGISTRY=${REGISTRY}"
echo "IMAGE=${IMAGE}"
echo "TAG=${TAG}"

if [[ "${MULTI_ARCH:-}" == "true" ]]; then
  make -f deployments/container/Makefile container-multi-arch \
    REGISTRY="${REGISTRY}" \
    IMAGE="${IMAGE}" \
    TAG="${TAG}"
else
  make -f deployments/container/Makefile docker-build \
    REGISTRY="${REGISTRY}" \
    IMAGE="${IMAGE}" \
    TAG="${TAG}"
fi
