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
	"io/fs"
	"math/rand"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/google/uuid"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const (
	tpuV4DeviceRegex        = `^accel[0-9]*$`
	tpuDeviceNumericalRegex = `^\d+$`
	defaultDeviceID         = "vfio"
	libtpuLogDir            = "/tmp/tpu_logs"
	DevicePluginPath        = "/var/lib/kubelet/plugins/tpu.google.com"
	LogDir                  = DevicePluginPath + "/logs"
)

type AllocatableDevices map[string]*AllocatableDevice

type AllocatableDevice struct {
	UUID          string `json:"uuid"`
	name          string
	index         int
	tpuGen        string
	brand         string
	driverVersion string
	allocatable   bool
}

// tpuManager manages google tpu devices.
type tpuManager struct {
	DevDirectory string
	tpuGen       string
	devices      AllocatableDevices
	tpuChipCount int
	nodeLabels   map[string]string
	envs         map[string]string
	commonMounts []*cdispec.Mount
	tpuLogDir    string
}

func NewTPUManager(nodeLabels map[string]string, devDirectory string) (*tpuManager, error) {
	accelerator := nodeLabels[AcceleratorLabel]
	acceleratorCount := nodeLabels[AcceleratorCountLabel]
	topology := nodeLabels[TopologyLabel]
	enableICIResiliency := nodeLabels[ICIResiliency]

	// Get TPU generation (v4, v5, etc.) and chips per node count from node labels.
	tpuGen, err := AcceleratorGen(nodeLabels[AcceleratorLabel])
	if err != nil {
		return nil, err
	}

	klog.Infof("Accelerator count from node labels: %s", acceleratorCount)
	chipCount, err := ChipCount(acceleratorCount)
	if err != nil {
		return nil, err
	}

	// Initialize environment variables that will be set in containers requesting TPU resources.
	// currently only support up to v6e which requires all tpu chips to be allocated to one container
	envs, err := InitEnvs(InitEnvOptions{
		Accelerator:         accelerator,
		Topology:            topology,
		ChipCount:           chipCount,
		AcceleratorCount:    chipCount,
		RequestedChipCount:  chipCount,
		EnableICIResiliency: enableICIResiliency,
	})
	if err != nil {
		return nil, fmt.Errorf("error initializing environment variables: %w", err)
	}

	commonMounts := []*cdispec.Mount{{
		HostPath:      libtpuLogDir,
		ContainerPath: libtpuLogDir,
		Options:       []string{"rw", "nosuid", "nodev", "bind"},
	},
	}

	return &tpuManager{
		tpuGen:       tpuGen,
		DevDirectory: devDirectory,
		tpuChipCount: chipCount,
		nodeLabels:   nodeLabels,
		envs:         envs,
		commonMounts: commonMounts,
		tpuLogDir:    libtpuLogDir,
	}, nil
}

func (d *AllocatableDevice) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name: d.name,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"index": {
				IntValue: ptr.To(int64(d.index)),
			},
			"uuid": {
				StringValue: ptr.To(d.UUID),
			},
			"tpuGen": {
				StringValue: ptr.To(d.tpuGen),
			},
		},
	}
	return device
}

func (tm *tpuManager) getTpuInfo(i int, f fs.DirEntry) *AllocatableDevice {
	seed := os.Getenv("NODE_NAME")
	uuid := tm.generateUUIDs(seed)
	allocatableDevice := &AllocatableDevice{
		UUID:  uuid,
		name:  f.Name(),
		index: i,
		// memoryBytes:   memory.Total, Question?
		tpuGen:        tm.tpuGen,
		brand:         "Google",
		driverVersion: "1.0.0",
		allocatable:   true,
	}
	return allocatableDevice
}

// Discovers all TPU devices available on the local node by walking tpuManager's devDirectory.
func (tm *tpuManager) enumerateAllPossibleTpuDevices() (AllocatableDevices, error) {
	klog.Info("Enumerating all possible Tpu Devices")
	var tpuDeviceRegex string
	switch tm.tpuGen {
	case "v3", "v4", "v4lite":
		tpuDeviceRegex = tpuV4DeviceRegex
	case "v5p", "v5lite", "v5litepod", "v6e":
		tpuDeviceRegex = tpuDeviceNumericalRegex
	}
	reg := regexp.MustCompile(tpuDeviceRegex)
	files, err := os.ReadDir(tm.DevDirectory)
	if err != nil {
		return nil, err
	}

	allocatableDevices := make(AllocatableDevices)
	num_devices := 0
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if reg.MatchString(f.Name()) {
			klog.Infof("Found Google TPU %q\n", f.Name())
			device := tm.getTpuInfo(num_devices, f)
			allocatableDevices[device.name] = device
			num_devices++
		}
	}
	// how to handle this gracefully?
	if num_devices != tm.tpuChipCount {
		return nil, fmt.Errorf("enumerated tpu devices not equal to chipCount")
	}
	klog.Info("Number of devices discovered", num_devices)
	tm.devices = allocatableDevices
	return allocatableDevices, nil
}

func (tm *tpuManager) generateUUIDs(seed string) string {
	rand := rand.New(rand.NewSource(hash(seed)))

	charset := make([]byte, 16)
	rand.Read(charset)
	uuid, _ := uuid.FromBytes(charset)
	return "tpu-" + uuid.String()
}

func hash(s string) int64 {
	h := int64(0)
	for _, c := range s {
		h = 31*h + int64(c)
	}
	return h
}

// ListDevices lists all physical TPU devices available on this node.
func (tm *tpuManager) ListDevices() AllocatableDevices {
	return tm.devices
}

// DeviceSpec returns the device spec that inclues list of devices to allocate for a deviceID.
func (tm *tpuManager) DeviceNodeContainerEdits(deviceID string) []*cdispec.DeviceNode {
	deviceNodes := make([]*cdispec.DeviceNode, 0)
	// default device mount
	deviceNodes = append(deviceNodes, &cdispec.DeviceNode{
		Path:        path.Join(tm.DevDirectory, deviceID),
		HostPath:    path.Join(tm.DevDirectory, deviceID),
		Permissions: "mrw",
	})
	// Currently v5 & v6 devices have this extra device that needs to be included.
	if strings.HasPrefix(tm.tpuGen, "v5") || strings.HasPrefix(tm.tpuGen, "v6e") {
		deviceNodes = append(deviceNodes, &cdispec.DeviceNode{
			Path:        path.Join(tm.DevDirectory, defaultDeviceID),
			HostPath:    path.Join(tm.DevDirectory, defaultDeviceID),
			Permissions: "mrw",
		})
	}
	return deviceNodes
}

func (tm *tpuManager) Envs() map[string]string {
	return tm.envs
}

// Validate the container requesting for TPUs. Make sure partial TPU chips are not requested.
func (tm *tpuManager) ValidateTpuRequest(requestDeviceIds []string) error {
	if len(requestDeviceIds) != tm.tpuChipCount {
		return fmt.Errorf("invalid TPU chip count request, you must request all %d chips on this node together", tm.tpuChipCount)
	}
	return nil
}
