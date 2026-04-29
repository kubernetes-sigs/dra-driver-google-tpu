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
SCRIPTS_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"

: ${DRIVER_NAME:=dra-driver-google-tpu}

# The registry, image and tag for the Google TPU DRA driver
: ${REGISTRY:="registry.k8s.io/dra-driver-google-tpu"}
: ${IMAGE:="${DRIVER_NAME}"}
: ${TAG:="v$(cat $(git rev-parse --show-toplevel)/deployments/helm/${DRIVER_NAME}/Chart.yaml | grep appVersion | sed 's/"//g' | sed -n 's/^appVersion: //p')"}

# The derived name of the driver image to build
: ${DRIVER_IMAGE:="${REGISTRY}/${IMAGE}:${TAG}"}

# Container tool, e.g. docker/podman
if [[ -z "${CONTAINER_TOOL}" ]]; then
    if [[ -n "$(which docker)" ]]; then
        echo "Docker found in PATH."
        CONTAINER_TOOL=docker
    elif [[ -n "$(which podman)" ]]; then
        echo "Podman found in PATH."
        CONTAINER_TOOL=podman
    else
        echo "No container tool detected. Please install Docker or Podman."
        return 1
    fi
fi

