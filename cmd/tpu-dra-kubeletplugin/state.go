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
	"context"
	"fmt"
	"slices"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	configapi "sigs.k8s.io/dra-driver-google-tpu/api/google.com/resource/tpu/v1alpha1"
)

type PreparedDevices []*PreparedDevice
type PreparedClaims map[string]PreparedDevices
type PerDeviceCDIContainerEdits map[string]*cdiapi.ContainerEdits

type PreparedDevice struct {
	kubeletplugin.Device
	ContainerEdits *cdiapi.ContainerEdits
}

func (pds PreparedDevices) GetDevices() []kubeletplugin.Device {
	var devices []kubeletplugin.Device
	for _, pd := range pds {
		devices = append(devices, pd.Device)
	}
	return devices
}

type DeviceState struct {
	sync.Mutex
	cdi               *CDIHandler
	allocatable       AllocatableDevices
	checkpointManager checkpointmanager.CheckpointManager
	tm                *tpuManager
	publishchan       chan interface{}
}

func NewDeviceState(config *Config, nodeLabels map[string]string, devDir string, publishChan chan interface{}) (*DeviceState, error) {
	klog.Info("Creating new DeviceState")

	tm, err := NewTPUManager(nodeLabels, devDir)
	if err != nil {
		panic(err)
	}
	allocatable, err := tm.enumerateAllPossibleTpuDevices()
	if err != nil {
		return nil, fmt.Errorf("error enumerating all possible devices: %v", err)
	}

	cdi, err := NewCDIHandler(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %v", err)
	}

	err = cdi.CreateCommonSpecFile(tm.envs, tm.commonMounts)
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for common edits: %v", err)
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %v", err)
	}

	state := &DeviceState{
		cdi:               cdi,
		allocatable:       allocatable,
		checkpointManager: checkpointManager,
		tm:                tm,
		publishchan:       publishChan,
	}

	checkpoints, err := state.checkpointManager.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("unable to list checkpoints: %v", err)
	}

	for _, c := range checkpoints {
		if c == DriverPluginCheckpointFile {
			return state, nil
		}
	}

	checkpoint := newCheckpoint()
	if err := state.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return state, nil
}

func (s *DeviceState) Prepare(ctx context.Context, claim *resourceapi.ResourceClaim) ([]kubeletplugin.Device, error) {
	s.Lock()
	defer s.Unlock()

	claimUID := string(claim.UID)

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync from checkpoint: %v", err)
	}

	preparedClaims := checkpoint.V1.PreparedClaims
	if preparedClaims[claimUID] != nil {
		klog.Infof("skip prepare: claim %v already exists in checkpoint", claimUID)
		return preparedClaims[claimUID].GetDevices(), nil
	}

	preparedDevices, err := s.prepareDevices(claim)
	if err != nil {
		return nil, fmt.Errorf("prepare failed for claim %v: %v", claimUID, err)
	}

	if err = s.cdi.CreateClaimSpecFile(claimUID, preparedDevices); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %v", err)
	}

	preparedClaims[claimUID] = preparedDevices
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return preparedClaims[claimUID].GetDevices(), nil
}

func (s *DeviceState) Unprepare(ctx context.Context, claimUID string) error {
	s.Lock()
	defer s.Unlock()

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync from checkpoint: %v", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] == nil {
		klog.Infof("unprepare skipped: claim %v not in checkpoint data", claimUID)
		return nil
	}

	if err := s.unprepareDevices(claimUID); err != nil {
		return fmt.Errorf("unprepare failed for claim %v: %v", claimUID, err)
	}

	err := s.cdi.DeleteClaimSpecFile(claimUID)
	if err != nil {
		return fmt.Errorf("unable to delete CDI spec file for claim: %v", err)
	}

	// unprepare succeeded, update the node local checkpoint claim data
	delete(preparedClaims, claimUID)
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return nil
}

func (s *DeviceState) prepareDevices(claim *resourceapi.ResourceClaim) (PreparedDevices, error) {
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim not yet allocated")
	}

	// A ResourceClaim can carry allocation results for more than one driver, so
	// take this driver's own results before counting or resolving anything. The
	// checks below are all about TPUs on this node: a device owned by another
	// driver is neither part of the chip count nor findable in s.allocatable,
	// and result.Driver is the only authoritative statement of ownership.
	var results []resourceapi.DeviceRequestAllocationResult
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != DriverName {
			continue
		}
		results = append(results, result)
	}

	if len(results) != s.tm.tpuChipCount {
		return nil, fmt.Errorf("invalid tpu resourceClaim, claim requests partial tpu devices (%d), only requests for all tpu devices (%d) on the node are supported", len(results), s.tm.tpuChipCount)
	}
	// Retrieve the full set of opaque device configs attached to the claim
	// and its device classes for this driver.
	configs, err := GetOpaqueDeviceConfigs(
		configapi.StrictDecoder,
		DriverName,
		claim.Status.Allocation.Devices.Config,
	)
	if err != nil {
		return nil, fmt.Errorf("error getting opaque device configs: %w", err)
	}

	// Add the default TPU config to the front of the config list with the
	// lowest precedence. This guarantees there will be at least one config in
	// the list with len(Requests) == 0 for the lookup below.
	configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{Config: configapi.DefaultTpuConfig()})

	// Look through the configs and figure out which one will be applied to
	// each device allocation result based on their order of precedence.
	configResultsMap := make(map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult)
	for _, result := range results {
		if _, exists := s.allocatable[result.Device]; !exists {
			return nil, fmt.Errorf("requested TPU is not allocatable: %v", result.Device)
		}
		for _, c := range slices.Backward(configs) {
			if len(c.Requests) == 0 || configMatchesRequest(c.Requests, result.Request) {
				configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
				break
			}
		}
	}

	// A config that decoded cleanly but matched no allocation result (for
	// example one bound to a misspelled request name, or fully shadowed by a
	// higher-precedence config) is silently unused. That mirrors the sibling
	// drivers -- erroring here would be wrong, since the named request may
	// legitimately have been satisfied by another driver -- but leave a trace
	// for debugging. configs[0] is the injected default and is exempt.
	for _, c := range configs[1:] {
		if _, used := configResultsMap[c.Config]; !used {
			klog.Warningf("TPU config for requests %v in claim %v matched no allocated TPU device and will be ignored", c.Requests, claim.UID)
		}
	}

	// Normalize, validate, and apply all configs associated with devices that
	// need to be prepared. Track container edits generated from applying the
	// config to the set of device allocation results.
	perDeviceCDIContainerEdits, err := s.getDeviceContainerEdits(configResultsMap)
	if err != nil {
		return nil, fmt.Errorf("error building container edits: %w", err)
	}

	// Walk through each device allocation result and construct the list of
	// prepared devices to return.
	var preparedDevices PreparedDevices
	for _, result := range results {
		device := &PreparedDevice{
			Device: kubeletplugin.Device{
				Requests:     []string{result.Request},
				PoolName:     result.Pool,
				DeviceName:   result.Device,
				CDIDeviceIDs: s.cdi.GetClaimDevices(string(claim.UID), []string{result.Device}),
			},
			ContainerEdits: perDeviceCDIContainerEdits[result.Device],
		}
		preparedDevices = append(preparedDevices, device)
	}

	return preparedDevices, nil
}

func (s *DeviceState) unprepareDevices(claimUID string) error {
	// remove all files in "/tmp/tpu_logs"
	// Removing all of the log files on un-prepare is fine as the expectation
	// is that a workload requests all TPUs. Revisit this if that assumption
	// changes.
	if err := RemoveDirContents(libtpuLogDir); err != nil {
		err = fmt.Errorf("failed to delete files in %s: %w", libtpuLogDir, err)
		return err
	}
	return nil
}

func (s *DeviceState) getDeviceContainerEdits(configResultsMap map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult) (PerDeviceCDIContainerEdits, error) {
	perDeviceEdits := make(PerDeviceCDIContainerEdits)

	claimEnv := make(map[string]string)
	for config, results := range configResultsMap {
		tpuConfig, ok := config.(*configapi.TpuConfig)
		if !ok {
			return nil, fmt.Errorf("runtime object is not a recognized configuration: %T", config)
		}

		envs, err := applyTpuConfig(tpuConfig)
		if err != nil {
			return nil, fmt.Errorf("error applying TPU config: %w", err)
		}

		if err := mergeClaimEnv(claimEnv, envs); err != nil {
			return nil, err
		}

		for _, result := range results {
			edits := &cdispec.ContainerEdits{
				DeviceNodes: s.tm.DeviceNodeContainerEdits(result.Device),
				Env:         envs,
			}
			perDeviceEdits[result.Device] = &cdiapi.ContainerEdits{ContainerEdits: edits}
		}
	}

	return perDeviceEdits, nil
}

func (s *DeviceState) UpdateHealth(deviceName string, currentHealth bool) error {
	var changed bool
	var oldHealth bool

	err := func() error {
		s.Lock()
		defer s.Unlock()

		device, ok := s.allocatable[deviceName]
		if !ok {
			return fmt.Errorf("device name %s does not exist in DeviceState", deviceName)
		}

		oldHealth = device.allocatable

		if oldHealth != currentHealth {
			// Update the local copy and write it back to the map
			device.allocatable = currentHealth
			s.allocatable[deviceName] = device

			changed = true
			klog.Infof("Device %s health changing: %v -> %v", deviceName, oldHealth, currentHealth)
		} else {
			klog.V(4).Infof("Device %s health already %v, no update needed", deviceName, currentHealth)
		}

		return nil
	}()

	if err != nil {
		klog.Errorf("Failed to update health for %s: %v", deviceName, err)
		return err
	}

	if changed {
		klog.Infof("Sending update signal to publishchan for device %s", deviceName)
		s.publishchan <- true
		klog.Infof("Update signal sent successfully for device %s", deviceName)
	}

	return nil
}
