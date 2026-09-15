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

package featuregates

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/version"
	logsapi "k8s.io/component-base/logs/api/v1"
)

func TestDriverGatesDefaultToDisabled(t *testing.T) {
	fg := newFeatureGates(featureGateEmulationVersion)

	if fg.Enabled(ConsumableShares) {
		t.Errorf("%s must default to disabled: enabling sharing changes what the driver "+
			"publishes for every node, so it has to be opted into explicitly", ConsumableShares)
	}
}

func TestRegistryHoldsDriverAndLoggingGates(t *testing.T) {
	fg := newFeatureGates(featureGateEmulationVersion)

	// KnownFeatures returns entries like "ConsumableShares=true|false (ALPHA - default=false)",
	// so match on the gate name prefix rather than the whole string.
	known := fg.KnownFeatures()
	hasGate := func(name string) bool {
		return slices.ContainsFunc(known, func(entry string) bool {
			return strings.HasPrefix(entry, name+"=")
		})
	}

	if !hasGate(string(ConsumableShares)) {
		t.Errorf("driver gate %s missing from registry; known = %v", ConsumableShares, known)
	}
	if !hasGate(string(logsapi.ContextualLogging)) {
		t.Errorf("logging gate %s missing from registry: driver and logging gates must share "+
			"one registry so a single --feature-gates flag configures both; known = %v",
			logsapi.ContextualLogging, known)
	}
}

func TestContextualLoggingOverrideIsApplied(t *testing.T) {
	fg := newFeatureGates(featureGateEmulationVersion)

	if !fg.Enabled(logsapi.ContextualLogging) {
		t.Errorf("%s must be enabled by default: dropping it would silently change logging behavior", logsapi.ContextualLogging)
	}
}

func TestContextualLoggingOverrideCanBeDisabled(t *testing.T) {
	fg := newFeatureGates(featureGateEmulationVersion)

	if err := fg.SetFromMap(map[string]bool{string(logsapi.ContextualLogging): false}); err != nil {
		t.Fatalf("failed to override %s to false: %v", logsapi.ContextualLogging, err)
	}
	if fg.Enabled(logsapi.ContextualLogging) {
		t.Errorf("%s should be disabled after SetFromMap with false", logsapi.ContextualLogging)
	}
}

// TestEmulationVersionMustBeKubernetesStyle pins down why
// featureGateEmulationVersion is a Kubernetes major.minor and not this driver's
// SemVer.
//
// component-base registers its logging gates at Kubernetes versions (v1.24+,
// v1.30+). Emulating a 0.x driver version makes those specs look newer than the
// emulated version, so they resolve to PreAlpha and the ContextualLogging
// override in newFeatureGates fails. The failure is a panic at startup by way
// of utilruntime.Must, so it would take the plugin down on every node.
func TestEmulationVersionMustBeKubernetesStyle(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a driver-SemVer emulation version to be rejected, but it was accepted; " +
				"if component-base has changed, revisit the featureGateEmulationVersion comment")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, string(logsapi.ContextualLogging)) {
			t.Errorf("expected the failure to name %s, got: %s", logsapi.ContextualLogging, msg)
		}
	}()

	newFeatureGates(version.MajorMinor(0, 4))
}

// TestDriverGateVisibleAtKubernetesEmulationVersion guards the other half of
// the same trade-off: the driver's own 0.x gates must remain settable while the
// registry emulates a 1.x Kubernetes version.
func TestDriverGateVisibleAtKubernetesEmulationVersion(t *testing.T) {
	fg := newFeatureGates(featureGateEmulationVersion)

	if err := fg.SetFromMap(map[string]bool{string(ConsumableShares): true}); err != nil {
		t.Fatalf("driver gate %s is not settable at emulation version %s: %v",
			ConsumableShares, featureGateEmulationVersion, err)
	}
	if !fg.Enabled(ConsumableShares) {
		t.Errorf("%s should be enabled after SetFromMap", ConsumableShares)
	}
}

func TestPackageLevelAccessors(t *testing.T) {
	if Enabled(ConsumableShares) {
		t.Errorf("%s must default to disabled via Enabled()", ConsumableShares)
	}

	known := KnownFeatures()
	if len(known) == 0 {
		t.Error("KnownFeatures() returned no features; registry must include both driver and logging gates")
	}
}
