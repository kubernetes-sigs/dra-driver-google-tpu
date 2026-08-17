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
	"encoding/json"
	"fmt"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/klog/v2"
)

const (
	// tpuDeviceStatusType is the discriminator in DeviceStatusData.Type,
	// mirroring the "type" field the GPU driver puts in its payload so a
	// consumer reading claims across drivers can dispatch on one key.
	tpuDeviceStatusType = "tpu"

	// The status write is best-effort and must never wedge Prepare: bound
	// each publish attempt.
	deviceStatusUpdateTimeout = 10 * time.Second
)

// DeviceStatusData is the opaque payload this driver publishes in
// ResourceClaim.status.devices[].data for every prepared TPU (KEP-4817). It
// mirrors the attributes exposed on the ResourceSlice so the identity of an
// allocated TPU stays visible on the claim even after the device disappears
// from the slice (for example when a health check marks it unallocatable).
type DeviceStatusData struct {
	Type   string `json:"type"`
	UUID   string `json:"uuid,omitempty"`
	TPUGen string `json:"tpuGen,omitempty"`
	Index  int    `json:"index"`
}

// buildDeviceStatuses returns one AllocatedDeviceStatus per allocation result
// owned by this driver. Results owned by other drivers are theirs to report.
//
// Must be called with the DeviceState lock held (it reads the allocatable
// device map).
func (s *DeviceState) buildDeviceStatuses(claim *resourceapi.ResourceClaim) []resourceapi.AllocatedDeviceStatus {
	if claim.Status.Allocation == nil {
		return nil
	}

	var statuses []resourceapi.AllocatedDeviceStatus
	// status.devices is validated by the API server as a set keyed by
	// (driver, pool, device, shareID); a duplicate key would make the whole
	// update be rejected.
	seen := make(map[string]bool)
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != DriverName {
			continue
		}
		device, ok := s.allocatable[result.Device]
		if !ok {
			continue
		}
		key := result.Pool + "/" + result.Device
		if result.ShareID != nil {
			key += "/" + string(*result.ShareID)
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		data, err := json.Marshal(DeviceStatusData{
			Type:   tpuDeviceStatusType,
			UUID:   device.UUID,
			TPUGen: device.tpuGen,
			Index:  device.index,
		})
		if err != nil {
			klog.Errorf("Failed to marshal device status data for %s: %v", result.Device, err)
			continue
		}
		statuses = append(statuses, resourceapi.AllocatedDeviceStatus{
			Driver:  result.Driver,
			Pool:    result.Pool,
			Device:  result.Device,
			ShareID: (*string)(result.ShareID),
			Data:    &k8sruntime.RawExtension{Raw: data},
		})
	}
	return statuses
}

// publishDeviceStatus writes this driver's device statuses to the claim.
// Failure is non-fatal and best-effort: the devices are prepared either way,
// the claim status is just missing the extra identity data. The write is
// retried only if the kubelet calls Prepare again for the claim (it does so
// after kubelet or plugin restarts and for every pod referencing the claim);
// a successful publish is remembered so those repeats skip the API round-trip.
//
// Must be called WITHOUT the DeviceState lock held: it blocks on the API
// server and the lock is shared with the health checker and slice publisher.
func (s *DeviceState) publishDeviceStatus(ctx context.Context, claim *resourceapi.ResourceClaim, statuses []resourceapi.AllocatedDeviceStatus) {
	if len(statuses) == 0 {
		return
	}
	claimUID := string(claim.UID)
	if s.statusPublished.Load(claimUID) {
		return
	}
	klog.Infof("Publishing device status for %d devices to ResourceClaim %s/%s", len(statuses), claim.Namespace, claim.Name)
	if err := s.updateDeviceStatus(ctx, claim, statuses); err != nil {
		klog.Errorf("Failed to update device status on ResourceClaim %s/%s: %v", claim.Namespace, claim.Name, err)
		return
	}
	s.statusPublished.Store(claimUID)
}

// updateDeviceStatus replaces this driver's entries in
// ResourceClaim.status.devices with statuses. A claim can carry statuses from
// several drivers; entries owned by other drivers are preserved (deliberately
// stricter than dra-example-driver, which overwrites status.devices wholesale).
func (s *DeviceState) updateDeviceStatus(ctx context.Context, claim *resourceapi.ResourceClaim, statuses []resourceapi.AllocatedDeviceStatus) error {
	ctx, cancel := context.WithTimeout(ctx, deviceStatusUpdateTimeout)
	defer cancel()

	rc := s.draclient.ResourceClaims(claim.Namespace)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := rc.Get(ctx, claim.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != claim.UID {
			// The claim was deleted and recreated under the same name since
			// it was prepared: the identities we hold belong to the old one.
			return fmt.Errorf("claim UID changed (prepared %s, found %s): not writing device status", claim.UID, current.UID)
		}
		// The vendored converting client's UpdateStatus has a bug on its
		// v1beta2 branch (it asserts the v1beta1 client to the v1beta2
		// interface and panics). The Get above negotiates and caches the API
		// version, so refuse here rather than crash the plugin on clusters
		// that serve resource.k8s.io/v1beta2 but not v1.
		if api := s.draclient.CurrentAPI(); api == "V1beta2" {
			return fmt.Errorf("device status updates are not supported through the %s API; the cluster must serve resource.k8s.io/v1", api)
		}

		merged := mergeDeviceStatuses(current.Status.Devices, statuses)
		if apiequality.Semantic.DeepEqual(current.Status.Devices, merged) {
			// Nothing to do; common on idempotent Prepare retries.
			return nil
		}
		current.Status.Devices = merged
		_, err = rc.UpdateStatus(ctx, current, metav1.UpdateOptions{})
		return err
	})
}

// mergeDeviceStatuses returns the entries of existing not owned by this
// driver, followed by ours (the full replacement set for this driver's
// entries).
func mergeDeviceStatuses(existing, ours []resourceapi.AllocatedDeviceStatus) []resourceapi.AllocatedDeviceStatus {
	var merged []resourceapi.AllocatedDeviceStatus
	for _, status := range existing {
		if status.Driver != DriverName {
			merged = append(merged, status)
		}
	}
	return append(merged, ours...)
}

// newDRAClient wraps the core clientset in the version-converting DRA client.
// Built once so the negotiated API version is cached across calls.
func newDRAClient(config *Config) *draclient.Client {
	if config == nil || config.coreclient == nil {
		return nil
	}
	return draclient.New(config.coreclient)
}
