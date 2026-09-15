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

// Package featuregates owns the single feature gate registry for this driver.
// It holds both the driver's own gates and the logging gates from
// k8s.io/component-base/logs, so that one --feature-gates flag configures
// everything.
package featuregates

import (
	"sync"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/featuregate"
	logsapi "k8s.io/component-base/logs/api/v1"
)

// featureGateEmulationVersion is the version passed to component-base's
// versioned feature gate. It must use Kubernetes-style versions (major.minor
// matching the Kubernetes release line), not this driver's SemVer.
//
// k8s.io/component-base/logs registers gates such as ContextualLogging at
// v1.24+/v1.30+. If emulation were the driver version (0.x), those specs would
// compare as newer than the emulation version, the gates would resolve to
// PreAlpha, and SetFromMap would fail (causing utilruntime.Must below to
// panic).
//
// The driver's own gates use 0.x Version fields and stay visible regardless,
// because 0.x < 1.y under apimachinery version ordering.
//
// Keep this aligned with the Kubernetes minor vendored in go.mod (k8s.io/*
// v0.X.x corresponds to K8s 1.X, currently 1.37) and bump it whenever those
// dependencies move to a new kube minor.
var featureGateEmulationVersion = version.MajorMinor(1, 37)

const (
	// ConsumableShares enables advertising TPU devices with
	// AllowMultipleAllocations and consumable capacity, so that more than one
	// ResourceClaim can be allocated the chips on a node.
	ConsumableShares featuregate.Feature = "ConsumableShares"
)

// defaultFeatureGates holds the driver's own gates. Version fields use the
// driver SemVer major.minor in which the gate was introduced.
//
// TODO: optionally isolate driver-only gates in their own registry if the driver
// needs independent lifecycle/deprecation tracking without sharing a single
// emulation version stream with component-base.
var defaultFeatureGates = map[featuregate.Feature]featuregate.VersionedSpecs{
	ConsumableShares: {
		{
			Default:    false,
			PreRelease: featuregate.Alpha,
			Version:    version.MajorMinor(0, 3),
		},
	},
}

var (
	featureGatesOnce sync.Once
	featureGates     featuregate.MutableVersionedFeatureGate
)

// FeatureGates returns the process-wide feature gate registry, creating it on
// first use.
func FeatureGates() featuregate.MutableVersionedFeatureGate {
	featureGatesOnce.Do(func() {
		featureGates = newFeatureGates(featureGateEmulationVersion)
	})
	return featureGates
}

// newFeatureGates builds a registry containing the driver gates and the
// component-base logging gates. Split out from FeatureGates so tests can build
// an isolated instance.
func newFeatureGates(v *version.Version) featuregate.MutableVersionedFeatureGate {
	fg := featuregate.NewVersionedFeatureGate(v)

	utilruntime.Must(logsapi.AddFeatureGates(fg))
	utilruntime.Must(fg.AddVersioned(defaultFeatureGates))

	// Contextual logging is enabled by default to establish recommended logging behavior.
	// Users can still override it via --feature-gates=ContextualLogging=false.
	loggingOverrides := map[string]bool{
		string(logsapi.ContextualLogging): true,
	}
	utilruntime.Must(fg.SetFromMap(loggingOverrides))

	return fg
}

// Enabled reports whether the named gate is enabled in the process-wide registry.
func Enabled(feature featuregate.Feature) bool {
	return FeatureGates().Enabled(feature)
}

// KnownFeatures returns the gates that may be set, for flag usage text.
func KnownFeatures() []string {
	return FeatureGates().KnownFeatures()
}
