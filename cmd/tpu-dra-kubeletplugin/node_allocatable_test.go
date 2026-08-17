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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/dra-driver-google-tpu/pkg/featuregates"
)

// expectedOverhead describes the expected parsed overhead for one resource;
// an empty string means the corresponding pointer must be nil.
type expectedOverhead struct {
	perPod       string
	perContainer string
}

// setNodeAllocatableGate sets the shared feature-gate registry for the test
// and restores the previous value on cleanup, so later tests in the package
// observe the documented default again.
func setNodeAllocatableGate(t *testing.T, enabled bool) {
	t.Helper()

	previous := featuregates.Enabled(featuregates.NodeAllocatableResources)
	set := func(value bool) {
		if err := featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.NodeAllocatableResources): value,
		}); err != nil {
			t.Fatalf("setting feature gate: %v", err)
		}
	}
	set(enabled)
	t.Cleanup(func() { set(previous) })
}

func checkOverheadQuantity(t *testing.T, name string, expected string, actual *resource.Quantity) {
	t.Helper()

	if expected == "" {
		if actual != nil {
			t.Errorf("%s: expected nil quantity, got %v", name, actual)
		}
		return
	}
	if actual == nil {
		t.Errorf("%s: expected %q, got nil", name, expected)
		return
	}
	if !resource.MustParse(expected).Equal(*actual) {
		t.Errorf("%s: expected %q, got %v", name, expected, actual)
	}
}

func TestNodeAllocatableOverheadsFromFlags(t *testing.T) {
	tests := []struct {
		name                     string
		featureGate              bool
		memoryPerPod             string
		memoryPerContainer       string
		cpuPerPod                string
		cpuPerContainer          string
		expectedNodeAllocatables map[corev1.ResourceName]expectedOverhead
		expectedErr              string
	}{
		{
			name:               "feature gate disabled with values set is an error",
			featureGate:        false,
			memoryPerPod:       "100Mi",
			memoryPerContainer: "10Mi",
			cpuPerPod:          "100m",
			cpuPerContainer:    "10m",
			expectedErr:        "feature gate NodeAllocatableResources is disabled",
		},
		{
			name:        "feature gate disabled with nothing configured is fine",
			featureGate: false,
		},
		{
			name:        "feature gate enabled but nothing configured",
			featureGate: true,
		},
		{
			name:         "memory per-pod only",
			featureGate:  true,
			memoryPerPod: "100Mi",
			expectedNodeAllocatables: map[corev1.ResourceName]expectedOverhead{
				corev1.ResourceMemory: {perPod: "100Mi"},
			},
		},
		{
			name:               "memory per-container only",
			featureGate:        true,
			memoryPerContainer: "10Mi",
			expectedNodeAllocatables: map[corev1.ResourceName]expectedOverhead{
				corev1.ResourceMemory: {perContainer: "10Mi"},
			},
		},
		{
			name:        "cpu per-pod only",
			featureGate: true,
			cpuPerPod:   "250m",
			expectedNodeAllocatables: map[corev1.ResourceName]expectedOverhead{
				corev1.ResourceCPU: {perPod: "250m"},
			},
		},
		{
			name:               "all four values",
			featureGate:        true,
			memoryPerPod:       "100Mi",
			memoryPerContainer: "10Mi",
			cpuPerPod:          "100m",
			cpuPerContainer:    "10m",
			expectedNodeAllocatables: map[corev1.ResourceName]expectedOverhead{
				corev1.ResourceMemory: {perPod: "100Mi", perContainer: "10Mi"},
				corev1.ResourceCPU:    {perPod: "100m", perContainer: "10m"},
			},
		},
		{
			name:               "zero values are accepted but omitted",
			featureGate:        true,
			memoryPerPod:       "0",
			memoryPerContainer: "0Mi",
			cpuPerPod:          "0",
			cpuPerContainer:    "0m",
		},
		{
			name:               "zero per-pod with positive per-container",
			featureGate:        true,
			memoryPerPod:       "0",
			memoryPerContainer: "10Mi",
			expectedNodeAllocatables: map[corev1.ResourceName]expectedOverhead{
				corev1.ResourceMemory: {perContainer: "10Mi"},
			},
		},
		{
			name:            "whitespace-only value is treated as unset",
			featureGate:     true,
			cpuPerContainer: "   ",
		},
		{
			name:         "memory configured but cpu unset yields only a memory entry",
			featureGate:  true,
			memoryPerPod: "100Mi",
			cpuPerPod:    "",
			expectedNodeAllocatables: map[corev1.ResourceName]expectedOverhead{
				corev1.ResourceMemory: {perPod: "100Mi"},
			},
		},
		{
			name:         "negative value is rejected",
			featureGate:  true,
			memoryPerPod: "-100Mi",
			expectedErr:  "invalid value for --node-allocatable-memory-overhead-per-pod: \"-100Mi\" (must not be negative)",
		},
		{
			name:        "unparseable value is rejected",
			featureGate: true,
			cpuPerPod:   "lots",
			expectedErr: "invalid value for --node-allocatable-cpu-overhead-per-pod: \"lots\"",
		},
		{
			name:         "invalid values are rejected even with the gate disabled",
			featureGate:  false,
			memoryPerPod: "lots",
			expectedErr:  "invalid value for --node-allocatable-memory-overhead-per-pod: \"lots\"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setNodeAllocatableGate(t, tc.featureGate)

			flags := &Flags{
				nodeAllocatableMemoryOverheadPerPod:       tc.memoryPerPod,
				nodeAllocatableMemoryOverheadPerContainer: tc.memoryPerContainer,
				nodeAllocatableCPUOverheadPerPod:          tc.cpuPerPod,
				nodeAllocatableCPUOverheadPerContainer:    tc.cpuPerContainer,
			}

			overheads, err := nodeAllocatableOverheadsFromFlags(flags)

			if tc.expectedErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("expected error containing %q, got %v", tc.expectedErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}

			if tc.expectedNodeAllocatables == nil {
				if overheads != nil {
					t.Fatalf("expected nil overheads, got %v", overheads)
				}
				return
			}

			if len(overheads) != len(tc.expectedNodeAllocatables) {
				t.Fatalf("expected %d entries, got %v", len(tc.expectedNodeAllocatables), overheads)
			}
			for name, expected := range tc.expectedNodeAllocatables {
				entry, ok := overheads[name]
				if !ok {
					t.Fatalf("expected an entry for %q", name)
				}
				if entry.Mapping != nil {
					t.Errorf("%s: Mapping must never be set, got %v", name, entry.Mapping)
				}
				if entry.Overhead == nil {
					t.Fatalf("%s: expected a non-nil Overhead", name)
				}
				checkOverheadQuantity(t, string(name)+" perPod", expected.perPod, entry.Overhead.PerPod)
				checkOverheadQuantity(t, string(name)+" perContainer", expected.perContainer, entry.Overhead.PerContainer)
			}
		})
	}
}

// The precomputed overhead must be attached to devices flowing through the
// ResourceSlice publish path, i.e. to the device built by
// AllocatableDevice.GetDevice(), and devices must stay untouched when nothing
// is configured.
func TestApplyNodeAllocatableOverheads(t *testing.T) {
	setNodeAllocatableGate(t, true)

	device := &AllocatableDevice{
		UUID:        "tpu-test",
		name:        "tpu-0",
		index:       0,
		tpuGen:      "v6e",
		allocatable: true,
	}

	dev := device.GetDevice()
	applyNodeAllocatableOverheads(&dev, &Config{})
	if dev.NodeAllocatableResources != nil {
		t.Errorf("nothing configured: expected nil NodeAllocatableResources, got %v", dev.NodeAllocatableResources)
	}

	overheads, err := nodeAllocatableOverheadsFromFlags(&Flags{
		nodeAllocatableMemoryOverheadPerPod: "100Mi",
	})
	if err != nil {
		t.Fatalf("parsing overhead flags: %v", err)
	}
	config := &Config{nodeAllocatableResources: overheads}

	dev = device.GetDevice()
	applyNodeAllocatableOverheads(&dev, config)

	if len(dev.NodeAllocatableResources) != 1 {
		t.Fatalf("expected exactly one NodeAllocatableResources entry, got %v", dev.NodeAllocatableResources)
	}
	entry, ok := dev.NodeAllocatableResources[corev1.ResourceMemory]
	if !ok {
		t.Fatal("expected a NodeAllocatableResources entry for memory")
	}
	if entry.Mapping != nil {
		t.Errorf("Mapping must never be set, got %v", entry.Mapping)
	}
	if entry.Overhead == nil {
		t.Fatal("expected a non-nil Overhead")
	}
	checkOverheadQuantity(t, "memory perPod", "100Mi", entry.Overhead.PerPod)
	checkOverheadQuantity(t, "memory perContainer", "", entry.Overhead.PerContainer)
}
