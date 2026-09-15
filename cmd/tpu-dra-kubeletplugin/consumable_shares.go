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
	"strconv"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-google-tpu/pkg/featuregates"
)

const (
	// consumableSharesDisabled is the default value of --consumable-shares.
	consumableSharesDisabled = "disabled"
	// consumableSharesUnlimited allows any number of claims to share the chips.
	consumableSharesUnlimited = "unlimited"

	// sharesCapacityName is the consumable capacity used to cap the number of
	// claims that may share a chip when --consumable-shares is an integer.
	sharesCapacityName resourceapi.QualifiedName = "shares"
)

// consumableSharesPolicy defines whether and how this node shares its TPU chips.
// It is resolved once at startup from the ConsumableShares feature gate, the
// --consumable-shares flag, and node topology.
//
// The zero value represents disabled sharing (the default behavior).
type consumableSharesPolicy struct {
	// enabled is true only when the ConsumableShares gate is on, the flag names
	// a sharing policy, and this node is single-host.
	enabled bool
	// unlimited is true when any number of claims may share the chips.
	unlimited bool
	// shares is the maximum number of claims that may share a chip.
	// Only meaningful when enabled is true and unlimited is false.
	shares int
}

// resolveConsumableShares determines the sharing policy for the node.
//
// Sharing is disabled on multi-host nodes regardless of flag settings, ensuring
// devices are published without sharing and Unprepare performs unconditional cleanup.
// Topology is inspected only when sharing is requested to avoid spurious warnings
// on unconfigured nodes.
func resolveConsumableShares(flag string, nodeLabels map[string]string) consumableSharesPolicy {
	if !featuregates.Enabled(featuregates.ConsumableShares) {
		return consumableSharesPolicy{}
	}

	flag = strings.TrimSpace(flag)
	if flag == "" || flag == consumableSharesDisabled {
		return consumableSharesPolicy{}
	}

	if !isSingleHostNode(nodeLabels) {
		klog.Warningf("--consumable-shares=%q ignored: TPU chip sharing is only supported "+
			"on single-host nodes, and this node is part of a multi-host slice. "+
			"Devices will be advertised without sharing.", flag)
		return consumableSharesPolicy{}
	}

	if flag == consumableSharesUnlimited {
		return consumableSharesPolicy{enabled: true, unlimited: true}
	}

	n, err := strconv.Atoi(flag)
	if err != nil || n <= 0 {
		return consumableSharesPolicy{}
	}
	return consumableSharesPolicy{enabled: true, shares: n}
}

// validateConsumableShares checks the flag value and its interaction with the
// feature gate during startup to fail fast on invalid configurations.
func validateConsumableShares(flag string) error {
	flag = strings.TrimSpace(flag)
	if flag == "" || flag == consumableSharesDisabled {
		return nil
	}

	if !featuregates.Enabled(featuregates.ConsumableShares) {
		return fmt.Errorf("--consumable-shares=%q requires feature gate %s (enable it via --feature-gates=%s=true)",
			flag, featuregates.ConsumableShares, featuregates.ConsumableShares)
	}

	if flag == consumableSharesUnlimited {
		return nil
	}

	n, err := strconv.Atoi(flag)
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid value for --consumable-shares: %q (must be %q, %q, or a positive integer)",
			flag, consumableSharesDisabled, consumableSharesUnlimited)
	}
	return nil
}

// applyConsumableShares annotates device capacity and allocation rules so the
// scheduler can co-allocate chips across claims.
func applyConsumableShares(dev *resourceapi.Device, policy consumableSharesPolicy) {
	if !policy.enabled {
		return
	}

	dev.AllowMultipleAllocations = new(true)

	// For unlimited sharing, AllowMultipleAllocations without capacity allows
	// unbounded claim allocations.
	if policy.unlimited {
		return
	}

	shares := *resource.NewQuantity(int64(policy.shares), resource.DecimalSI)
	one := *resource.NewQuantity(1, resource.DecimalSI)

	if dev.Capacity == nil {
		dev.Capacity = make(map[resourceapi.QualifiedName]resourceapi.DeviceCapacity)
	}
	dev.Capacity[sharesCapacityName] = resourceapi.DeviceCapacity{
		Value: shares,
		RequestPolicy: &resourceapi.CapacityRequestPolicy{
			Default: new(one.DeepCopy()),
			ValidRange: &resourceapi.CapacityRequestPolicyRange{
				Min:  new(one.DeepCopy()),
				Max:  new(shares.DeepCopy()),
				Step: new(one.DeepCopy()),
			},
		},
	}
}
