/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"slices"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const foreignDriverName = "example.com/nic"

func tpuDeviceState(chipCount int, deviceNames ...string) *DeviceState {
	allocatable := AllocatableDevices{}
	for i, name := range deviceNames {
		allocatable[name] = &AllocatableDevice{
			UUID:        name,
			name:        name,
			index:       i,
			allocatable: true,
		}
	}
	return &DeviceState{
		cdi:         &CDIHandler{},
		allocatable: allocatable,
		tm: &tpuManager{
			DevDirectory: "/dev",
			devices:      allocatable,
			tpuChipCount: chipCount,
		},
	}
}

func claimWithResults(results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid"},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: results},
			},
		},
	}
}

func claimWithResultsAndConfigs(configs []resourceapi.DeviceAllocationConfiguration, results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	claim := claimWithResults(results...)
	claim.Status.Allocation.Devices.Config = configs
	return claim
}

func result(driver, pool, device, request string) resourceapi.DeviceRequestAllocationResult {
	return resourceapi.DeviceRequestAllocationResult{
		Driver:  driver,
		Pool:    pool,
		Device:  device,
		Request: request,
	}
}

// A ResourceClaim can be satisfied by more than one driver. The TPU plugin must
// apply its full-chip count, its allocatable lookup and its CDI edits to the
// results it owns, and leave the rest alone.
func TestPrepareDevicesIgnoresForeignAllocationResults(t *testing.T) {
	tests := []struct {
		name        string
		state       *DeviceState
		results     []resourceapi.DeviceRequestAllocationResult
		wantErr     bool
		wantDevices []string
	}{
		{
			name:  "all results owned by this driver",
			state: tpuDeviceState(2, "tpu0", "tpu1"),
			results: []resourceapi.DeviceRequestAllocationResult{
				result(DriverName, "tpu-pool", "tpu0", "tpus"),
				result(DriverName, "tpu-pool", "tpu1", "tpus"),
			},
			wantDevices: []string{"tpu0", "tpu1"},
		},
		{
			// Without the ownership filter the foreign entry pushes the result
			// count past tpuChipCount, so a complete TPU allocation is rejected
			// because the workload also asked for a NIC.
			name:  "full chip set alongside a device owned by another driver",
			state: tpuDeviceState(2, "tpu0", "tpu1"),
			results: []resourceapi.DeviceRequestAllocationResult{
				result(DriverName, "tpu-pool", "tpu0", "tpus"),
				result(DriverName, "tpu-pool", "tpu1", "tpus"),
				result(foreignDriverName, "nic-pool", "nic0", "nics"),
			},
			wantDevices: []string{"tpu0", "tpu1"},
		},
		{
			// Device names are only unique within a driver's pools. A foreign
			// device that happens to be called "tpu0" must not be resolved
			// against this driver's allocatable map or prepared as a TPU.
			name:  "another driver uses a device name that also exists here",
			state: tpuDeviceState(2, "tpu0", "tpu1"),
			results: []resourceapi.DeviceRequestAllocationResult{
				result(DriverName, "tpu-pool", "tpu0", "tpus"),
				result(DriverName, "tpu-pool", "tpu1", "tpus"),
				result(foreignDriverName, "nic-pool", "tpu0", "nics"),
			},
			wantDevices: []string{"tpu0", "tpu1"},
		},
		{
			// The partial-allocation error still has to fire. A foreign result
			// must not stand in for a missing TPU and make the count add up.
			name:  "partial TPU allocation padded by a foreign result",
			state: tpuDeviceState(2, "tpu0", "tpu1"),
			results: []resourceapi.DeviceRequestAllocationResult{
				result(DriverName, "tpu-pool", "tpu0", "tpus"),
				result(foreignDriverName, "nic-pool", "nic0", "nics"),
			},
			wantErr: true,
		},
		{
			name:  "only foreign results",
			state: tpuDeviceState(2, "tpu0", "tpu1"),
			results: []resourceapi.DeviceRequestAllocationResult{
				result(foreignDriverName, "nic-pool", "nic0", "nics"),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := tt.state.prepareDevices(claimWithResults(tt.results...))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("prepareDevices() = %d devices, want an error", len(prepared))
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareDevices() error = %v, want nil", err)
			}
			if len(prepared) != len(tt.wantDevices) {
				t.Fatalf("prepareDevices() prepared %d devices, want %d", len(prepared), len(tt.wantDevices))
			}
			got := make(map[string]bool, len(prepared))
			for _, device := range prepared {
				got[device.DeviceName] = true
			}
			for _, want := range tt.wantDevices {
				if !got[want] {
					t.Errorf("prepareDevices() did not prepare %q; prepared %v", want, got)
				}
			}
		})
	}
}

// Opaque device configs attached to the claim or its device classes must be
// decoded, resolved by precedence, and turned into CDI environment edits on
// every prepared device.
func TestPrepareDevicesAppliesOpaqueConfigs(t *testing.T) {
	tests := []struct {
		name    string
		configs []resourceapi.DeviceAllocationConfiguration
		// Allocation results for the claim; defaults to tpu0 and tpu1 both
		// allocated to a single request named "tpus".
		results []resourceapi.DeviceRequestAllocationResult
		wantErr bool
		// Environment entries every prepared device must carry.
		wantEnvs []string
	}{
		{
			name:     "no configs applies the default config with no env edits",
			wantEnvs: nil,
		},
		{
			name: "claim config sets libtpu log levels",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(2)),
			},
			wantEnvs: []string{"TPU_MIN_LOG_LEVEL=2", "TPU_STDERR_LOG_LEVEL=2"},
		},
		{
			name: "claim config overrides class config",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClass, DriverName, tpuConfigJSON(0)),
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(3)),
			},
			wantEnvs: []string{"TPU_MIN_LOG_LEVEL=3", "TPU_STDERR_LOG_LEVEL=3"},
		},
		{
			name: "config bound to another request leaves this claim's devices alone",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(1), "nics"),
			},
			wantEnvs: nil,
		},
		{
			name: "config bound to the claim's request applies",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(1), "tpus"),
			},
			wantEnvs: []string{"TPU_MIN_LOG_LEVEL=1", "TPU_STDERR_LOG_LEVEL=1"},
		},
		{
			name: "config for another driver is ignored",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, foreignDriverName, []byte(`{"not": "a tpu config"}`)),
			},
			wantEnvs: nil,
		},
		{
			name: "config with an out-of-range level fails prepare",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(7)),
			},
			wantErr: true,
		},
		{
			name: "config with an unknown field fails prepare",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, []byte(`{"apiVersion": "tpu.resource.google.com/v1alpha1", "kind": "TpuConfig", "bogus": true}`)),
			},
			wantErr: true,
		},
		{
			// The env vars a TpuConfig produces act on the whole container.
			// Two requests in one claim with configs that disagree on a value
			// would make the merged container environment depend on CDI
			// device application order, so prepare must fail instead.
			name: "configs on different requests with conflicting values fail prepare",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(1), "tpus"),
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(2), "moretpus"),
			},
			results: []resourceapi.DeviceRequestAllocationResult{
				result(DriverName, "tpu-pool", "tpu0", "tpus"),
				result(DriverName, "tpu-pool", "tpu1", "moretpus"),
			},
			wantErr: true,
		},
		{
			name: "configs on different requests with agreeing values apply",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(1), "tpus"),
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(1), "moretpus"),
			},
			results: []resourceapi.DeviceRequestAllocationResult{
				result(DriverName, "tpu-pool", "tpu0", "tpus"),
				result(DriverName, "tpu-pool", "tpu1", "moretpus"),
			},
			wantEnvs: []string{"TPU_MIN_LOG_LEVEL=1", "TPU_STDERR_LOG_LEVEL=1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := tpuDeviceState(2, "tpu0", "tpu1")
			results := tt.results
			if results == nil {
				results = []resourceapi.DeviceRequestAllocationResult{
					result(DriverName, "tpu-pool", "tpu0", "tpus"),
					result(DriverName, "tpu-pool", "tpu1", "tpus"),
				}
			}
			claim := claimWithResultsAndConfigs(tt.configs, results...)

			prepared, err := state.prepareDevices(claim)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("prepareDevices() = %d devices, want an error", len(prepared))
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareDevices() error = %v, want nil", err)
			}
			if len(prepared) != 2 {
				t.Fatalf("prepareDevices() prepared %d devices, want 2", len(prepared))
			}
			for _, device := range prepared {
				if device.ContainerEdits == nil || device.ContainerEdits.ContainerEdits == nil {
					t.Fatalf("device %q has no container edits", device.DeviceName)
				}
				edits := device.ContainerEdits.ContainerEdits
				if len(edits.DeviceNodes) == 0 {
					t.Errorf("device %q lost its device-node edits", device.DeviceName)
				}
				if len(edits.Env) != len(tt.wantEnvs) {
					t.Errorf("device %q env = %v, want %v", device.DeviceName, edits.Env, tt.wantEnvs)
					continue
				}
				for _, want := range tt.wantEnvs {
					if !slices.Contains(edits.Env, want) {
						t.Errorf("device %q env = %v, missing %q", device.DeviceName, edits.Env, want)
					}
				}
			}
		})
	}
}
