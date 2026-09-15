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
KIND_SCRIPTS_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"

# Source the global common variables
source "${KIND_SCRIPTS_DIR}/../../../scripts/common.sh"

# The kubernetes repo to build the kind cluster from
: ${KIND_K8S_REPO:="https://github.com/kubernetes/kubernetes.git"}

# The kubernetes tag to build the kind cluster from
# From ${KIND_K8S_REPO}/tags
: ${KIND_K8S_TAG:="v1.37.0"}

# At present, kind has a new enough node image that we don't need to build our
# own. This won't always be true and we may need to set the variable below to
# 'true' from time to time as things change.
: ${BUILD_KIND_IMAGE:="false"}

# The name of the kind cluster to create
: ${KIND_CLUSTER_NAME:="${DRIVER_NAME}-cluster"}

# The path to kind's cluster configuration file
: ${KIND_CLUSTER_CONFIG_PATH:="${KIND_SCRIPTS_DIR}/kind-cluster-config.yaml"}

# The name of the kind image to build / run
: ${KIND_IMAGE:="kindest/node:${KIND_K8S_TAG}"}

: ${KIND:="env KIND_EXPERIMENTAL_PROVIDER=${CONTAINER_TOOL} kind"}
