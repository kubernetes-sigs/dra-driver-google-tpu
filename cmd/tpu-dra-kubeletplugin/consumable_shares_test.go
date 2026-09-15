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

	resourceapi "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/dump"

	"sigs.k8s.io/dra-driver-google-tpu/pkg/featuregates"
)

// withConsumableSharesGate flips the process-wide gate for the duration of a
// test. These tests must not run in parallel with each other because of it.
func withConsumableSharesGate(t *testing.T, enabled bool) {
	t.Helper()
	fg := featuregates.FeatureGates()
	previous := fg.Enabled(featuregates.ConsumableShares)
	if err := fg.SetFromMap(map[string]bool{string(featuregates.ConsumableShares): enabled}); err != nil {
		t.Fatalf("setting %s=%v: %v", featuregates.ConsumableShares, enabled, err)
	}
	t.Cleanup(func() {
		if err := fg.SetFromMap(map[string]bool{string(featuregates.ConsumableShares): previous}); err != nil {
			t.Fatalf("restoring %s=%v: %v", featuregates.ConsumableShares, previous, err)
		}
	})
}

// TestResolveConsumableShares covers the full cross product of the three
// conditions that gate sharing: the feature gate, the flag, and whether the
// node is single-host. All three must hold before any device is advertised as
// shareable.
func TestResolveConsumableShares(t *testing.T) {
	singleHostLabels := map[string]string{
		AcceleratorLabel: "tpu-v5-lite-device",
	}
	multiHostLabels := map[string]string{
		AcceleratorLabel:      "tpu-v4-podslice",
		TopologyLabel:         "4x4x4",
		AcceleratorCountLabel: "4",
	}

	for _, tc := range []struct {
		name   string
		gate   bool
		flag   string
		labels map[string]string
		want   consumableSharesPolicy
	}{
		// Gate off: nothing else can turn sharing on.
		{
			name:   "gate off, flag unlimited",
			gate:   false,
			flag:   consumableSharesUnlimited,
			labels: singleHostLabels,
			want:   consumableSharesPolicy{},
		},
		{
			name:   "gate off, flag integer",
			gate:   false,
			flag:   "4",
			labels: singleHostLabels,
			want:   consumableSharesPolicy{},
		},
		{
			name:   "gate off, flag disabled",
			gate:   false,
			flag:   consumableSharesDisabled,
			labels: singleHostLabels,
			want:   consumableSharesPolicy{},
		},

		// Gate on, single-host: the flag selects the policy.
		{
			name:   "disabled",
			gate:   true,
			flag:   consumableSharesDisabled,
			labels: singleHostLabels,
			want:   consumableSharesPolicy{},
		},
		{
			name:   "empty flag",
			gate:   true,
			flag:   "",
			labels: singleHostLabels,
			want:   consumableSharesPolicy{},
		},
		{
			name:   "unlimited",
			gate:   true,
			flag:   consumableSharesUnlimited,
			labels: singleHostLabels,
			want:   consumableSharesPolicy{enabled: true, unlimited: true},
		},
		{
			name:   "integer 1",
			gate:   true,
			flag:   "1",
			labels: singleHostLabels,
			want:   consumableSharesPolicy{enabled: true, shares: 1},
		},
		{
			name:   "integer 4",
			gate:   true,
			flag:   "4",
			labels: singleHostLabels,
			want:   consumableSharesPolicy{enabled: true, shares: 4},
		},
		{
			name:   "whitespace tolerated",
			gate:   true,
			flag:   "  4  ",
			labels: singleHostLabels,
			want:   consumableSharesPolicy{enabled: true, shares: 4},
		},

		{
			name:   "invalid value",
			gate:   true,
			flag:   "invalid",
			labels: singleHostLabels,
			want:   consumableSharesPolicy{},
		},
		{
			name:   "negative integer",
			gate:   true,
			flag:   "-4",
			labels: singleHostLabels,
			want:   consumableSharesPolicy{},
		},
		{
			name:   "zero integer",
			gate:   true,
			flag:   "0",
			labels: singleHostLabels,
			want:   consumableSharesPolicy{},
		},

		// Multi-host: sharing is refused whatever the flag says.
		{
			name:   "multi-host, unlimited",
			gate:   true,
			flag:   consumableSharesUnlimited,
			labels: multiHostLabels,
			want:   consumableSharesPolicy{},
		},
		{
			name:   "multi-host, integer",
			gate:   true,
			flag:   "4",
			labels: multiHostLabels,
			want:   consumableSharesPolicy{},
		},
		{
			name:   "multi-host, disabled",
			gate:   true,
			flag:   consumableSharesDisabled,
			labels: multiHostLabels,
			want:   consumableSharesPolicy{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withConsumableSharesGate(t, tc.gate)

			got := resolveConsumableShares(tc.flag, tc.labels)
			if got != tc.want {
				t.Errorf("resolveConsumableShares(%q, labels=%v) = %+v, want %+v",
					tc.flag, tc.labels, got, tc.want)
			}
		})
	}
}

func TestValidateConsumableShares(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gate    bool
		flag    string
		wantErr bool
	}{
		{
			name:    "disabled without gate",
			gate:    false,
			flag:    consumableSharesDisabled,
			wantErr: false,
		},
		{
			name:    "empty without gate",
			gate:    false,
			flag:    "",
			wantErr: false,
		},
		{
			name:    "unlimited without gate",
			gate:    false,
			flag:    consumableSharesUnlimited,
			wantErr: true,
		},
		{
			name:    "integer without gate",
			gate:    false,
			flag:    "4",
			wantErr: true,
		},

		{
			name:    "disabled with gate",
			gate:    true,
			flag:    consumableSharesDisabled,
			wantErr: false,
		},
		{
			name:    "unlimited with gate",
			gate:    true,
			flag:    consumableSharesUnlimited,
			wantErr: false,
		},
		{
			name:    "integer with gate",
			gate:    true,
			flag:    "4",
			wantErr: false,
		},

		{
			name:    "zero",
			gate:    true,
			flag:    "0",
			wantErr: true,
		},
		{
			name:    "negative",
			gate:    true,
			flag:    "-1",
			wantErr: true,
		},
		{
			name:    "not a number",
			gate:    true,
			flag:    "abc",
			wantErr: true,
		},
		{
			name:    "float",
			gate:    true,
			flag:    "1.5",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withConsumableSharesGate(t, tc.gate)

			err := validateConsumableShares(tc.flag)
			if tc.wantErr && err == nil {
				t.Errorf("validateConsumableShares(%q) with gate=%v: expected an error, got nil",
					tc.flag, tc.gate)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateConsumableShares(%q) with gate=%v: unexpected error: %v",
					tc.flag, tc.gate, err)
			}
			if tc.wantErr && err != nil && !tc.gate {
				if want := "--feature-gates=" + string(featuregates.ConsumableShares) + "=true"; !strings.Contains(err.Error(), want) {
					t.Errorf("validateConsumableShares(%q) error = %q; want actionable guidance %q", tc.flag, err, want)
				}
			}
		})
	}
}

func TestApplyConsumableShares(t *testing.T) {
	for _, tc := range []struct {
		name   string
		device *resourceapi.Device
		policy consumableSharesPolicy
		want   *resourceapi.Device
	}{
		{
			name:   "disabled policy leaves the device untouched",
			device: &resourceapi.Device{Name: "0"},
			policy: consumableSharesPolicy{},
			want:   &resourceapi.Device{Name: "0"},
		},
		{
			name:   "unlimited sets multi-allocation with no capacity",
			device: &resourceapi.Device{Name: "0"},
			policy: consumableSharesPolicy{enabled: true, unlimited: true},
			want: &resourceapi.Device{
				Name:                     "0",
				AllowMultipleAllocations: new(true),
			},
		},
		{
			name:   "integer publishes a shares capacity",
			device: &resourceapi.Device{Name: "0"},
			policy: consumableSharesPolicy{enabled: true, shares: 4},
			want: &resourceapi.Device{
				Name:                     "0",
				AllowMultipleAllocations: new(true),
				Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
					sharesCapacityName: {
						Value: *resource.NewQuantity(4, resource.DecimalSI),
						RequestPolicy: &resourceapi.CapacityRequestPolicy{
							Default: resource.NewQuantity(1, resource.DecimalSI),
							ValidRange: &resourceapi.CapacityRequestPolicyRange{
								Min:  resource.NewQuantity(1, resource.DecimalSI),
								Max:  resource.NewQuantity(4, resource.DecimalSI),
								Step: resource.NewQuantity(1, resource.DecimalSI),
							},
						},
					},
				},
			},
		},
		{
			name: "existing capacity is preserved",
			device: &resourceapi.Device{
				Name: "0",
				Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
					"memory": {Value: resource.MustParse("16Gi")},
				},
			},
			policy: consumableSharesPolicy{enabled: true, shares: 2},
			want: &resourceapi.Device{
				Name:                     "0",
				AllowMultipleAllocations: new(true),
				Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
					"memory": {Value: resource.MustParse("16Gi")},
					sharesCapacityName: {
						Value: *resource.NewQuantity(2, resource.DecimalSI),
						RequestPolicy: &resourceapi.CapacityRequestPolicy{
							Default: resource.NewQuantity(1, resource.DecimalSI),
							ValidRange: &resourceapi.CapacityRequestPolicyRange{
								Min:  resource.NewQuantity(1, resource.DecimalSI),
								Max:  resource.NewQuantity(2, resource.DecimalSI),
								Step: resource.NewQuantity(1, resource.DecimalSI),
							},
						},
					},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applyConsumableShares(tc.device, tc.policy)
			if !apiequality.Semantic.DeepEqual(tc.device, tc.want) {
				t.Errorf("applyConsumableShares() got:\n%s\nwant:\n%s",
					dump.Pretty(tc.device), dump.Pretty(tc.want))
			}
		})
	}
}
