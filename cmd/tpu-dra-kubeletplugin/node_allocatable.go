/*
 * Copyright The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/dra-driver-google-tpu/pkg/featuregates"
)

// overheadQuantity parses one configured overhead value. It returns nil for
// unset and zero values (omitted from published ResourceSlices) and an error
// for unparseable or negative values.
func overheadQuantity(flagName, value string) (*resource.Quantity, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, fmt.Errorf("invalid value for %s: %q: %w", flagName, value, err)
	}
	if quantity.Sign() < 0 {
		return nil, fmt.Errorf("invalid value for %s: %q (must not be negative)", flagName, value)
	}
	if quantity.IsZero() {
		return nil, nil
	}
	return &quantity, nil
}

// nodeAllocatableOverhead builds the overhead for a single resource from its
// per-pod and per-container flag values, or nil when neither is positive.
func nodeAllocatableOverhead(perPodFlag, perPod, perContainerFlag, perContainer string) (*resourceapi.NodeAllocatableOverhead, error) {
	perPodQuantity, err := overheadQuantity(perPodFlag, perPod)
	if err != nil {
		return nil, err
	}
	perContainerQuantity, err := overheadQuantity(perContainerFlag, perContainer)
	if err != nil {
		return nil, err
	}
	if perPodQuantity == nil && perContainerQuantity == nil {
		return nil, nil
	}
	return &resourceapi.NodeAllocatableOverhead{
		PerPod:       perPodQuantity,
		PerContainer: perContainerQuantity,
	}, nil
}

// nodeAllocatableOverheadsFromFlags parses the overhead flags once, at
// startup, into the NodeAllocatableResources map published on every device.
// Unparseable or negative values are an error, as is configuring overhead
// values while the NodeAllocatableResources feature gate is disabled. The
// result is nil when nothing is configured. Only the Overhead branch is ever
// set: TPUs are not node resources, so there is nothing to express via
// Mapping.
func nodeAllocatableOverheadsFromFlags(flags *Flags) (map[corev1.ResourceName]resourceapi.NodeAllocatableResource, error) {
	memory, err := nodeAllocatableOverhead(
		"--node-allocatable-memory-overhead-per-pod", flags.nodeAllocatableMemoryOverheadPerPod,
		"--node-allocatable-memory-overhead-per-container", flags.nodeAllocatableMemoryOverheadPerContainer,
	)
	if err != nil {
		return nil, err
	}
	cpu, err := nodeAllocatableOverhead(
		"--node-allocatable-cpu-overhead-per-pod", flags.nodeAllocatableCPUOverheadPerPod,
		"--node-allocatable-cpu-overhead-per-container", flags.nodeAllocatableCPUOverheadPerContainer,
	)
	if err != nil {
		return nil, err
	}

	if memory == nil && cpu == nil {
		return nil, nil
	}
	if !featuregates.Enabled(featuregates.NodeAllocatableResources) {
		return nil, fmt.Errorf("node-allocatable overhead flags are set but feature gate %s is disabled; enable the gate or unset the overhead flags", featuregates.NodeAllocatableResources)
	}

	overheads := make(map[corev1.ResourceName]resourceapi.NodeAllocatableResource)
	if memory != nil {
		overheads[corev1.ResourceMemory] = resourceapi.NodeAllocatableResource{Overhead: memory}
	}
	if cpu != nil {
		overheads[corev1.ResourceCPU] = resourceapi.NodeAllocatableResource{Overhead: cpu}
	}
	return overheads, nil
}

// applyNodeAllocatableOverheads attaches the precomputed overhead entries to a
// published device. The map is built once at startup and never mutated
// afterwards, so sharing it across devices is safe.
func applyNodeAllocatableOverheads(dev *resourceapi.Device, config *Config) {
	if len(config.nodeAllocatableResources) == 0 {
		return
	}
	dev.NodeAllocatableResources = config.nodeAllocatableResources
}
