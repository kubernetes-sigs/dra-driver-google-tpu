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

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
)

const (
	// DRA Driver settings.
	devDirectory     = "/dev"
	devDirectoryVfio = "/dev/vfio"
)

type driver struct {
	client        coreclientset.Interface
	pluginHelper  *kubeletplugin.Helper
	deviceState   *DeviceState
	healthchecker *TPUHealthChecker
	// health fans device health reports out to WatchHealthStatus
	// subscriptions (see device_health_status.go).
	health *healthBroadcaster
	config *Config
}

func NewDriver(ctx context.Context,
	config *Config) (*driver, error) {

	err := ApplyNetworkSettings()
	if err != nil {
		klog.Errorf("error applying network settings: %v", err)
	}

	// Fetch node labels.
	nodeLabels, err := getTPUNodeLabels(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("failed to get TPU node labels: %w", err)
	}

	model := nodeLabels[AcceleratorLabel]
	if model == "" {
		return nil, fmt.Errorf("node label %s must be set", AcceleratorLabel)
	}

	tpuGen, err := AcceleratorGen(model)
	if err != nil {
		return nil, fmt.Errorf("error fetching accelerator generation: %w", err)
	}

	// Determine device directory based on TPU generation.
	var devDir string
	switch tpuGen {
	case "v3", "v4", "v4lite":
		devDir = devDirectory
	case "v5p", "v5lite", "v5litepod", "v6e":
		devDir = devDirectoryVfio
	}

	triggerPublishChan := make(chan interface{}, 1)
	state, err := NewDeviceState(config, nodeLabels, devDir, triggerPublishChan)
	if err != nil {
		return nil, err
	}

	health := newHealthBroadcaster(state, config.flags.nodeName)

	hc := NewTPUHealthChecker(state.allocatable, state, devDir, tpuGen, health)
	if err = hc.Start(); err != nil {
		return nil, fmt.Errorf("healthchecker failed to start")
	}

	driver := &driver{
		client:        config.coreclient,
		deviceState:   state,
		config:        config,
		healthchecker: hc,
		health:        health,
	}

	helper, err := kubeletplugin.Start(
		ctx,
		driver,
		kubeletplugin.KubeClient(config.coreclient),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(DriverName),
		// Ensures PrepareResourceClaim and UnprepareResourceClaim calls are called serially.
		// This means we don't have to handle race conditions as functions are called one at a time.
		kubeletplugin.Serialize(true),
		kubeletplugin.RegistrarDirectoryPath(config.flags.kubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.DriverPluginPath()),
		// KEP-4680: report the health of allocated TPUs to the kubelet so it
		// surfaces in pod.status.containerStatuses[].allocatedResourcesStatus.
		kubeletplugin.HealthService(true),
	)
	if err != nil {
		return nil, err
	}
	driver.pluginHelper = helper

	if err = driver.GatherStateAndPublish(ctx); err != nil {
		return nil, err
	}

	// Anyone can trigger republish of resourceSlice by writing to the channel.
	go driver.republishSlice(ctx, triggerPublishChan)

	klog.Info("Published resourceslice")
	return driver, nil
}

func (d *driver) GatherStateAndPublish(ctx context.Context) error {
	// Read the DeviceState and build a new resourcev1.ResourceSlice
	var resourceSlice resourceslice.Slice
	func() {
		d.deviceState.Lock()
		defer d.deviceState.Unlock()

		for _, device := range d.deviceState.allocatable {
			if device.allocatable {
				klog.Info("Appending Device", device)
				dev := device.GetDevice()
				applyNodeAllocatableOverheads(&dev, d.config)
				resourceSlice.Devices = append(resourceSlice.Devices, dev)
			}
		}
	}()

	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			d.config.flags.nodeName: {Slices: []resourceslice.Slice{resourceSlice}},
		},
	}
	if err := d.pluginHelper.PublishResources(ctx, resources); err != nil {
		return err
	}

	return nil
}

// republishSlice waits for channel notification to trigger republishing of slice.
func (d *driver) republishSlice(ctx context.Context, publishObjectChan <-chan interface{}) {
	klog.Info("republish resource slices now ready.")
	defer klog.Info("republish resource slices has terminated.")

	for {
		select {
		case <-ctx.Done():
			klog.Info("context cancelled, terminating republishSlices routine")
			return
		case _, ok := <-publishObjectChan:
			if !ok {
				klog.Info("channel is closed, terminating republishSlices routine")
				return
			}
			// Call GatherDevicesAndPublish.
			if err := d.GatherStateAndPublish(ctx); err != nil {
				klog.Errorf("republish operation failed: %v", err)
			}
		}
	}
}

func (d *driver) Shutdown(ctx context.Context) error {
	if d == nil {
		return nil
	}
	if d.healthchecker != nil {
		d.healthchecker.Stop()
	}
	d.pluginHelper.Stop()
	return nil
}

func (d *driver) HandleError(ctx context.Context, err error, msg string) {
	// For now we just follow the advice documented in the DRAPlugin API docs.
	// See: https://pkg.go.dev/k8s.io/apimachinery/pkg/util/runtime#HandleErrorWithContext
	runtime.HandleErrorWithContext(ctx, err, msg)
}

func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (result map[types.UID]kubeletplugin.PrepareResult, err error) {
	klog.Infof("PrepareResource is called: number of claims: %d", len(claims))
	prepareResults := make(map[types.UID]kubeletplugin.PrepareResult)

	for _, claim := range claims {
		prepareResults[claim.UID] = d.nodePrepareResource(ctx, claim)
	}

	return prepareResults, nil
}

func (d *driver) nodePrepareResource(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	prepared, err := d.deviceState.Prepare(ctx, claim)
	if err != nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error preparing devices for claimID %v, claimName %s: %v", claim.UID, claim.Name, err),
		}
	}

	klog.Infof("Returning newly prepared devices for claim '%v': %v", claim.UID, prepared)
	return kubeletplugin.PrepareResult{Devices: prepared}
}

func (d *driver) UnprepareResourceClaims(ctx context.Context, claimRefs []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	klog.Infof("UnPrepareResource is called: number of claims: %d", len(claimRefs))
	unpreparedResults := make(map[types.UID]error)

	for _, claimRef := range claimRefs {
		unpreparedResults[claimRef.UID] = d.nodeUnprepareResource(ctx, claimRef)
	}

	return unpreparedResults, nil
}

func (d *driver) nodeUnprepareResource(ctx context.Context, claimRef kubeletplugin.NamespacedObject) error {
	if err := d.deviceState.Unprepare(ctx, string(claimRef.UID)); err != nil {
		return fmt.Errorf("error unpreparing devices for claim %v: %v", claimRef.UID, err)
	}

	return nil
}
