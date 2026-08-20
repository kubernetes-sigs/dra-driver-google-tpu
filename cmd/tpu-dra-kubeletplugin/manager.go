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
	// Properties of the TPU the chip belongs to, what a claim selects on.
	accelerator string
	chipCount   int
	topology    string
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
		return nil, fmt.Errorf("cannot determine the number of TPU chips of the node, set --tpu-chip-count or the %s label: %w", AcceleratorCountLabel, err)
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
			"brand": {
				StringValue: ptr.To(d.brand),
			},
			"accelerator": {
				StringValue: ptr.To(d.accelerator),
			},
			"chipCount": {
				IntValue: ptr.To(int64(d.chipCount)),
			},
		},
	}
	// A node of a multi host slice cannot always tell its topology.
	if d.topology != "" {
		device.Attributes["topology"] = resourceapi.DeviceAttribute{
			StringValue: ptr.To(d.topology),
		}
	}
	return device
}

func (tm *tpuManager) getTpuInfo(i int, f fs.DirEntry) *AllocatableDevice {
	uuid := tm.generateUUIDs(deviceUUIDSeed(os.Getenv("NODE_NAME"), f.Name()))
	allocatableDevice := &AllocatableDevice{
		UUID:  uuid,
		name:  f.Name(),
		index: i,
		// memoryBytes:   memory.Total, Question?
		tpuGen:        tm.tpuGen,
		brand:         "Google",
		driverVersion: "1.0.0",
		allocatable:   true,
		accelerator:   tm.nodeLabels[AcceleratorLabel],
		chipCount:     tm.tpuChipCount,
		topology:      tm.nodeLabels[TopologyLabel],
	}
	return allocatableDevice
}

// Discovers all TPU devices available on the local node by walking tpuManager's devDirectory.
func (tm *tpuManager) enumerateAllPossibleTpuDevices() (AllocatableDevices, error) {
	klog.Info("Enumerating all possible Tpu Devices")
	// The device naming depends on the kernel interface the chips are bound to,
	// not on the TPU generation, which may be unknown to this driver.
	reg := regexp.MustCompile(tpuV4DeviceRegex)
	if tm.DevDirectory == devDirectoryVfio {
		reg = regexp.MustCompile(tpuDeviceNumericalRegex)
	}
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
		return nil, fmt.Errorf("found %d TPU devices in %s, the node is expected to have %d chips", num_devices, tm.DevDirectory, tm.tpuChipCount)
	}
	klog.Info("Number of devices discovered", num_devices)
	tm.devices = allocatableDevices
	return allocatableDevices, nil
}

// deviceUUIDSeed returns the seed used to derive a chip's UUID. TPUs expose no
// hardware UUID, so the UUID is derived deterministically from the node name and
// the chip's device file name (e.g. "accel0" or "0"), which is what identifies a
// chip on the host and is also the ResourceSlice device name. This makes the
// UUID unique per chip on a node and stable across plugin restarts.
func deviceUUIDSeed(nodeName, deviceName string) string {
	return nodeName + "/" + deviceName
}

// generateUUIDs derives a deterministic "tpu-<uuid>" string from seed.
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
