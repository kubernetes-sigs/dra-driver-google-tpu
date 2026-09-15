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
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
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
	sharesPolicy      consumableSharesPolicy
	tpuLogDir         string
}

func (s *DeviceState) logDir() string {
	if s.tpuLogDir != "" {
		return s.tpuLogDir
	}
	if s.tm != nil && s.tm.tpuLogDir != "" {
		return s.tm.tpuLogDir
	}
	return libtpuLogDir
}

func NewDeviceState(config *Config, nodeLabels map[string]string, devDir string, publishChan chan interface{}, sharesPolicy consumableSharesPolicy) (*DeviceState, error) {
	klog.Info("Creating new DeviceState")

	tm, err := NewTPUManager(nodeLabels, devDir)
	if err != nil {
		return nil, fmt.Errorf("error creating the TPU manager: %w", err)
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
		sharesPolicy:      sharesPolicy,
		tpuLogDir:         tm.tpuLogDir,
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

	if err := s.unprepareDevices(claimUID, checkpoint); err != nil {
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
	// Look through the configs and figure out which one will be applied to
	// each device allocation result based on their order of precedence.
	for _, result := range results {
		if _, exists := s.allocatable[result.Device]; !exists {
			return nil, fmt.Errorf("requested TPU is not allocatable: %v", result.Device)
		}
	}

	// Normalize, validate, and apply all configs associated wi th devices that
	// need to be prepared. Track container edits generated from applying the
	// config to the set of device allocation results.
	perDeviceCDIContainerEdits, err := s.getDeviceContainerEdits(results)
	if err != nil {
		klog.Error(err)
	}
	// Walk through each config and its associated device allocation results
	// and construct the list of prepared devices to return.
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

// unprepareDevices tears down the node-level state that this claim was using.
//
// The libtpu log directory is a single host path shared by every claim on the
// node, and it is tailed by the log collector sidecar. Removing its contents on
// every unprepare is only safe while a single claim can hold the chips. When
// consumable shares is enabled, the contents are removed by whichever claim is
// the last one out.
func (s *DeviceState) unprepareDevices(claimUID string, checkpoint *Checkpoint) error {
	logDir := s.logDir()
	if s.sharesPolicy.enabled && hasOtherPreparedClaims(checkpoint, claimUID) {
		klog.V(4).Infof("unprepare: TPU chips are still held by other claims, keeping %s for claim %v",
			logDir, claimUID)
		return nil
	}

	// remove all files in the libtpu log directory
	if err := RemoveDirContents(logDir); err != nil {
		return fmt.Errorf("failed to delete files in %s: %w", logDir, err)
	}
	return nil
}

// hasOtherPreparedClaims reports whether a claim other than claimUID is still
// checkpointed as prepared.
//
// Every claim on a TPU node is allocated every chip on it, so the number of
// prepared claims is a sufficient reference count for the chips; there is no
// need to track which claim holds which device. Unprepare deletes the claim
// from the checkpoint only after teardown succeeds, so the claim being torn
// down is still present here and has to be skipped explicitly.
func hasOtherPreparedClaims(checkpoint *Checkpoint, claimUID string) bool {
	if checkpoint == nil || checkpoint.V1 == nil {
		return false
	}
	for uid := range checkpoint.V1.PreparedClaims {
		if uid != claimUID {
			return true
		}
	}
	return false
}

func (s *DeviceState) getDeviceContainerEdits(results []resourceapi.DeviceRequestAllocationResult) (PerDeviceCDIContainerEdits, error) {
	perDeviceEdits := make(PerDeviceCDIContainerEdits)

	for _, result := range results {
		deviceNodes := s.tm.DeviceNodeContainerEdits(result.Device)
		edits := &cdispec.ContainerEdits{
			DeviceNodes: deviceNodes,
		}

		perDeviceEdits[result.Device] = &cdiapi.ContainerEdits{ContainerEdits: edits}
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
