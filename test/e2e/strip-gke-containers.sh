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

# Helm post-renderer used by the kind e2e tests. The chart's DaemonSet bundles
# a GKE-only network-optimizer init container and the vbar-control-agent /
# log-collector sidecars, which need real GKE infrastructure. Strip them so
# only the tpu-dra-plugin container remains; everything else in the rendered
# release is passed through untouched. Applying this at render time (instead
# of patching the DaemonSet after helm) keeps every helm upgrade a single
# rollout, which the seamless-upgrade test relies on.

set -eu -o pipefail

: ${YQ:="yq"}

${YQ} 'with(select(.kind == "DaemonSet");
    del(.spec.template.spec.initContainers) |
    .spec.template.spec.containers |= map(select(.name == "tpu-dra-plugin")))'
