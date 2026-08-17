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

// Package featuregates defines the driver-level feature gates of the TPU DRA
// driver. These are independent of the Kubernetes cluster feature gates: a
// driver gate controls whether this driver uses a feature, while the cluster
// must separately enable any Kubernetes gates the feature depends on.
package featuregates

import (
	"github.com/spf13/pflag"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"
)

// FeatureGate is a mutable feature-gate registry that is also usable as a
// command-line flag value.
type FeatureGate interface {
	featuregate.MutableVersionedFeatureGate
	pflag.Value
}

const (
	// NodeAllocatableResources enables publishing node-allocatable overhead
	// (KEP-5517) for TPU devices in ResourceSlices. Overhead values are
	// configured via the --node-allocatable-*-overhead-per-* flags of the
	// kubelet plugin. The cluster must have the Kubernetes feature gate
	// DRANodeAllocatableResources enabled for the API server to accept the
	// published field.
	NodeAllocatableResources featuregate.Feature = "NodeAllocatableResources"
)

var defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	NodeAllocatableResources: {
		Default:    false,
		PreRelease: featuregate.Alpha,
	},
}

var featureGates FeatureGate

func init() {
	fg := featuregate.NewFeatureGate()
	utilruntime.Must(fg.Add(defaultFeatureGates))
	featureGates = fg
}

// FeatureGates returns the mutable feature gate registry of the driver.
func FeatureGates() FeatureGate {
	return featureGates
}

// Enabled reports whether the given driver feature gate is enabled.
func Enabled(f featuregate.Feature) bool {
	return featureGates.Enabled(f)
}
