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
	"slices"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/dynamic-resource-allocation/resourceclaim"

	configapi "sigs.k8s.io/dra-driver-google-tpu/api/google.com/resource/tpu/v1alpha1"
)

// OpaqueDeviceConfig is a decoded opaque config along with the list of
// requests it applies to. An empty Requests list means the config applies to
// all requests in the claim.
type OpaqueDeviceConfig struct {
	Requests []string
	Config   runtime.Object
}

// GetOpaqueDeviceConfigs returns an ordered list of the configs contained in possibleConfigs for this driver.
//
// Configs can either come from the resource claim itself or from the device
// class associated with the request. Configs coming directly from the resource
// claim take precedence over configs coming from the device class. Moreover,
// configs found later in the list of configs attached to its source take
// precedence over configs found earlier in the list for that source.
//
// All of the configs relevant to the driver from the list of possibleConfigs
// will be returned in order of precedence (from lowest to highest). If no
// configs are found, nil is returned.
func GetOpaqueDeviceConfigs(
	decoder runtime.Decoder,
	driverName string,
	possibleConfigs []resourceapi.DeviceAllocationConfiguration,
) ([]*OpaqueDeviceConfig, error) {
	// Collect all configs in order of reverse precedence.
	var classConfigs []resourceapi.DeviceAllocationConfiguration
	var claimConfigs []resourceapi.DeviceAllocationConfiguration
	var candidateConfigs []resourceapi.DeviceAllocationConfiguration
	for _, config := range possibleConfigs {
		switch config.Source {
		case resourceapi.AllocationConfigSourceClass:
			classConfigs = append(classConfigs, config)
		case resourceapi.AllocationConfigSourceClaim:
			claimConfigs = append(claimConfigs, config)
		default:
			return nil, fmt.Errorf("invalid config source: %v", config.Source)
		}
	}
	candidateConfigs = append(candidateConfigs, classConfigs...)
	candidateConfigs = append(candidateConfigs, claimConfigs...)

	// Decode all configs that are relevant for the driver.
	var resultConfigs []*OpaqueDeviceConfig
	for _, config := range candidateConfigs {
		// If this is nil, the driver doesn't support some future API extension
		// and needs to be updated.
		if config.Opaque == nil {
			return nil, fmt.Errorf("only opaque parameters are supported by this driver")
		}

		// Configs for different drivers may have been specified because a
		// single request can be satisfied by different drivers. This is not
		// an error -- drivers must skip over other driver's configs in order
		// to support this.
		if config.Opaque.Driver != driverName {
			continue
		}

		decodedConfig, err := runtime.Decode(decoder, config.Opaque.Parameters.Raw)
		if err != nil {
			return nil, fmt.Errorf("error decoding config parameters: %w", err)
		}

		resultConfig := &OpaqueDeviceConfig{
			Requests: config.Requests,
			Config:   decodedConfig,
		}

		resultConfigs = append(resultConfigs, resultConfig)
	}

	return resultConfigs, nil
}

// configMatchesRequest reports whether a config that names configRequests
// applies to the allocation result for resultRequest. A config that names
// only a main request also applies to all of its subrequests, which appear
// in results as "<main request>/<subrequest>".
func configMatchesRequest(configRequests []string, resultRequest string) bool {
	if slices.Contains(configRequests, resultRequest) {
		return true
	}
	if baseRequestRef := resourceclaim.BaseRequestRef(resultRequest); baseRequestRef != resultRequest {
		return slices.Contains(configRequests, baseRequestRef)
	}
	return false
}

// applyTpuConfig normalizes and validates a TpuConfig and translates it into
// the CDI environment variable edits (consumed by libtpu) to attach to every
// device the config applies to. The settings in a TpuConfig are
// container-wide rather than tied to a single chip, so the same entries are
// returned for all of the config's devices.
func applyTpuConfig(config *configapi.TpuConfig) ([]string, error) {
	// Normalize the config to set any implied defaults.
	if err := config.Normalize(); err != nil {
		return nil, fmt.Errorf("error normalizing TPU config: %w", err)
	}

	// Validate the config to ensure its integrity.
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("error validating TPU config: %w", err)
	}

	var envs []string
	if config.Logging != nil {
		if config.Logging.Level != nil {
			envs = append(envs, fmt.Sprintf("TPU_MIN_LOG_LEVEL=%d", *config.Logging.Level))
		}
		if config.Logging.StderrLevel != nil {
			envs = append(envs, fmt.Sprintf("TPU_STDERR_LOG_LEVEL=%d", *config.Logging.StderrLevel))
		}
	}
	return envs, nil
}

// mergeClaimEnv folds one config's environment entries into the claim-wide
// view in seen. Because these variables act on the whole container, two
// configs in the same claim that disagree on a value would leave the merged
// container environment dependent on the order the runtime applies CDI
// devices in, so conflicting values are rejected.
func mergeClaimEnv(seen map[string]string, envs []string) error {
	for _, env := range envs {
		key, value, _ := strings.Cut(env, "=")
		if previous, ok := seen[key]; ok && previous != value {
			return fmt.Errorf("conflicting TPU configs within the claim: %s is set to both %q and %q, but it applies to the whole container", key, previous, value)
		}
		seen[key] = value
	}
	return nil
}
