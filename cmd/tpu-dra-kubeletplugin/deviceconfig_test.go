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
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"

	configapi "sigs.k8s.io/dra-driver-google-tpu/api/google.com/resource/tpu/v1alpha1"
)

func tpuConfigJSON(level int32) []byte {
	return fmt.Appendf(nil,
		`{"apiVersion": %q, "kind": %q, "logging": {"level": %d}}`,
		configapi.GroupName+"/"+configapi.Version, configapi.TpuConfigKind, level)
}

func opaqueConfig(source resourceapi.AllocationConfigSource, driver string, parameters []byte, requests ...string) resourceapi.DeviceAllocationConfiguration {
	return resourceapi.DeviceAllocationConfiguration{
		Source:   source,
		Requests: requests,
		DeviceConfiguration: resourceapi.DeviceConfiguration{
			Opaque: &resourceapi.OpaqueDeviceConfiguration{
				Driver:     driver,
				Parameters: runtime.RawExtension{Raw: parameters},
			},
		},
	}
}

func loggingLevel(t *testing.T, config runtime.Object) int32 {
	t.Helper()
	tpuConfig, ok := config.(*configapi.TpuConfig)
	if !ok {
		t.Fatalf("decoded config is %T, want *TpuConfig", config)
	}
	if tpuConfig.Logging == nil || tpuConfig.Logging.Level == nil {
		t.Fatal("decoded config has no logging level set")
	}
	return *tpuConfig.Logging.Level
}

func TestGetOpaqueDeviceConfigs(t *testing.T) {
	tests := []struct {
		name    string
		configs []resourceapi.DeviceAllocationConfiguration
		wantErr string
		// Logging levels of the returned configs, in precedence order
		// (lowest first).
		wantLevels []int32
	}{
		{
			name:       "no configs",
			wantLevels: nil,
		},
		{
			name: "single claim config",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(1)),
			},
			wantLevels: []int32{1},
		},
		{
			// Class configs come first (lowest precedence) even when listed
			// after claim configs, and within a source later entries take
			// precedence over earlier ones.
			name: "claim configs take precedence over class configs",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(2)),
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(3)),
				opaqueConfig(resourceapi.AllocationConfigSourceClass, DriverName, tpuConfigJSON(0)),
				opaqueConfig(resourceapi.AllocationConfigSourceClass, DriverName, tpuConfigJSON(1)),
			},
			wantLevels: []int32{0, 1, 2, 3},
		},
		{
			name: "configs for other drivers are skipped",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, foreignDriverName, []byte(`{"not": "a tpu config"}`)),
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName, tpuConfigJSON(2)),
			},
			wantLevels: []int32{2},
		},
		{
			name: "non-opaque config is rejected",
			configs: []resourceapi.DeviceAllocationConfiguration{
				{Source: resourceapi.AllocationConfigSourceClaim},
			},
			wantErr: "only opaque parameters are supported",
		},
		{
			name: "invalid config source is rejected",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig("NotASource", DriverName, tpuConfigJSON(1)),
			},
			wantErr: "invalid config source",
		},
		{
			name: "unknown field is rejected by strict decoding",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName,
					fmt.Appendf(nil, `{"apiVersion": %q, "kind": %q, "loggingg": {"level": 1}}`,
						configapi.GroupName+"/"+configapi.Version, configapi.TpuConfigKind)),
			},
			wantErr: "error decoding config parameters",
		},
		{
			name: "unknown kind is rejected",
			configs: []resourceapi.DeviceAllocationConfiguration{
				opaqueConfig(resourceapi.AllocationConfigSourceClaim, DriverName,
					fmt.Appendf(nil, `{"apiVersion": %q, "kind": "NotATpuConfig"}`,
						configapi.GroupName+"/"+configapi.Version)),
			},
			wantErr: "error decoding config parameters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configs, err := GetOpaqueDeviceConfigs(configapi.StrictDecoder, DriverName, tt.configs)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("GetOpaqueDeviceConfigs() = %d configs, want error containing %q", len(configs), tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("GetOpaqueDeviceConfigs() error = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetOpaqueDeviceConfigs() error = %v, want nil", err)
			}
			if len(configs) != len(tt.wantLevels) {
				t.Fatalf("GetOpaqueDeviceConfigs() returned %d configs, want %d", len(configs), len(tt.wantLevels))
			}
			for i, want := range tt.wantLevels {
				if got := loggingLevel(t, configs[i].Config); got != want {
					t.Errorf("config[%d] logging level = %d, want %d", i, got, want)
				}
			}
		})
	}
}

func TestConfigMatchesRequest(t *testing.T) {
	tests := []struct {
		name           string
		configRequests []string
		resultRequest  string
		want           bool
	}{
		{
			name:           "exact match",
			configRequests: []string{"tpus"},
			resultRequest:  "tpus",
			want:           true,
		},
		{
			name:           "no match",
			configRequests: []string{"tpus"},
			resultRequest:  "nics",
			want:           false,
		},
		{
			name:           "main request matches its subrequests",
			configRequests: []string{"tpus"},
			resultRequest:  "tpus/subrequest",
			want:           true,
		},
		{
			name:           "exact subrequest match",
			configRequests: []string{"tpus/subrequest"},
			resultRequest:  "tpus/subrequest",
			want:           true,
		},
		{
			name:           "different subrequest of the same main request",
			configRequests: []string{"tpus/other"},
			resultRequest:  "tpus/subrequest",
			want:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := configMatchesRequest(tt.configRequests, tt.resultRequest); got != tt.want {
				t.Errorf("configMatchesRequest(%v, %q) = %v, want %v", tt.configRequests, tt.resultRequest, got, tt.want)
			}
		})
	}
}
