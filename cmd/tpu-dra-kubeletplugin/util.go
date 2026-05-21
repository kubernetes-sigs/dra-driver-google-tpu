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
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

const (
	AcceleratorLabel             = "cloud.google.com/gke-tpu-accelerator"
	AcceleratorCountLabel        = "cloud.google.com/gke-accelerator-count"
	AcceleratorTopologyModeLabel = "cloud.google.com/gke-accelerator-topology-mode"
	TopologyLabel                = "cloud.google.com/gke-tpu-topology"
	ICIResiliency                = "cloud.google.com/gke-tpu-ici-resiliency"
	twist                        = "false"
	vlpMaxTopologyDim            = 16
	ICIResiliencyEnv             = "ENABLE_ICI_RESILIENCY"
	ProvisionOnlyTopologyMode    = "PROVISION_ONLY"
	RootDirectory                = "/"
	NodeIPEnv                    = "NODE_IP"
)

var (
	acceleratorRegex     = regexp.MustCompile(`^tpu\d+[a-z]?$`)
	pastAcceleratorRegex = regexp.MustCompile(`^tpu-v\d+([ep]?-slice)?((?:-lite)?-(device|podslice))?$`)

	// The tpugen's value should follow the format of CloudTPU "TPU_ACCELERATOR_TYPE" : relative ascending order of release.
	validTPUGenerations = map[string]int{
		"v3":        0,
		"v4":        1,
		"v4lite":    2,
		"v5lite":    3,
		"v5litepod": 4,
		"v5p":       5,
		"v6e":       6,
	}
	// chips per node -> chips per dimension.
	requestedChipCountToChipsPerDimNumaAligned = map[int][]int64{
		1: {1, 1, 1},
		2: {1, 2, 1},
		4: {2, 2, 1},
		8: {2, 4, 1},
	}

	networkSettings = []SystemSetting{
		{FilePath: "proc/sys/net/ipv4/tcp_slow_start_after_idle", Value: "0"},
		{FilePath: "proc/sys/net/ipv4/tcp_no_metrics_save", Value: "1"},
		{FilePath: "sys/module/tcp_cubic/parameters/hystart_detect", Value: "2"},
		{FilePath: "proc/sys/net/core/somaxconn", Value: "4096"},
		{FilePath: "proc/sys/net/ipv4/tcp_max_syn_backlog", Value: "4096"},
		{FilePath: "proc/sys/net/ipv4/tcp_mtu_probing", Value: "0"},
		{FilePath: "proc/sys/net/core/optmem_max", Value: "131072"},
	}

	validSubsliceTopologySet = map[string]bool{
		"1x1":   true,
		"2x2":   true,
		"2x4":   true,
		"4x4":   true,
		"4x8":   true,
		"8x8":   true,
		"8x16":  true,
		"16x16": true,
		"4x4x4": true,
		"2x4x4": true,
		"2x2x4": true,
		"2x2x2": true,
		"2x2x1": true,
	}
)

// InitEnvOptions contains fields that are required to initiliazing the
// environment variables used by tpu-device-plugin.
type InitEnvOptions struct {
	Accelerator           string
	Topology              string
	EnableICIResiliency   string
	ChipCount             int
	AcceleratorCount      int
	RequestedChipCount    int
	SubSliceTopology      string
	IsPriviledged         bool
	VisibleChipIds        []string
	EnableDeviceSpreading bool
	NumaNodeIds           []string
}

// SystemSetting contains filePath and its setting value.
type SystemSetting struct {
	FilePath string
	Value    string
}

func RemoveDirContents(dirPath string) error {
	dir, _ := os.ReadDir(dirPath)
	for _, d := range dir {
		if err := os.RemoveAll(path.Join([]string{dirPath, d.Name()}...)); err != nil {
			return fmt.Errorf("failed deleting: %s, error %w", d.Name(), err)
		}
	}
	return nil
}

func IsValidSubSliceTopology(topology string, subSliceTopology string) (bool, error) {
	// subSliceTopology not specified
	if subSliceTopology == "" {
		return false, nil
	}
	// subSliceTopology not valid
	if _, ok := validSubsliceTopologySet[subSliceTopology]; !ok {
		return false, fmt.Errorf("invalid value for subSliceTopology: %s", subSliceTopology)
	}

	// Normalize node topology
	dims, err := getTopologyDims(topology)
	if err != nil {
		return false, fmt.Errorf("invalid node topology %s: %w", topology, err)
	}

	// Normalize subslice topology
	subDims, err := getTopologyDims(subSliceTopology)
	if err != nil {
		return false, fmt.Errorf("invalid value for subSliceTopology %s: %w", subSliceTopology, err)
	}

	for i := range dims {
		d1 := dims[i]    // From control plane (normalized)
		d2 := subDims[i] // From user input (normalized)
		if d2 > d1 {
			return false, fmt.Errorf("invalid value for subSliceTopology: %s, subSliceTopology shouldn't be larger than topology", subSliceTopology)
		}
	}
	return true, nil
}

// InitEnvs initializes a map of environment variables containing required
// metadata values for TPU workloads to run.
func InitEnvs(opts InitEnvOptions) (map[string]string, error) {
	// Get accelerator generation (v4, v5, etc.) and topology dimensions from node labels.
	tpuGen, err := AcceleratorGen(opts.Accelerator)
	if err != nil {
		return nil, err
	}
	var topology string
	valid, err := IsValidSubSliceTopology(opts.Topology, opts.SubSliceTopology)
	if valid {
		topology = opts.SubSliceTopology
	} else {
		topology = opts.Topology
	}
	if err != nil {
		klog.Errorf("Invalid subSliceTopology: %v, setting the topology env as %s.", err, topology)
	}

	var topologyDims []int64
	var acceleratorTypeConverted string
	envs := map[string]string{
		"TPU_SKIP_MDS_QUERY": "true",
	}

	if topology != "" {
		topologyDims, err = getTopologyDims(topology)
		if err != nil {
			return nil, err
		}
		// Convert accelerator type to <tpuGeneration>-<numCores> format used by GKE.
		acceleratorTypeConverted, err = convertAcceleratorType(tpuGen, topologyDims)
		if err != nil {
			return nil, err
		}
		envs["TPU_TOPOLOGY"] = topology
		envs["TPU_ACCELERATOR_TYPE"] = acceleratorTypeConverted
	}

	nodeIp, err := GetEnvName(NodeIPEnv)
	if err != nil {
		klog.Infof("$NODE_IP is not set in env")
	} else {
		envs["VBAR_CONTROL_SERVICE_URL"] = nodeIp + ":8353"
	}

	if opts.EnableDeviceSpreading && opts.IsPriviledged && len(opts.VisibleChipIds) > 0 {
		envs["TPU_VISIBLE_CHIPS"] = strings.Join(opts.VisibleChipIds, ",")
	}

	if len(opts.NumaNodeIds) > 0 {
		envs["WORKLOAD_NIC_PREFERRED_NUMA"] = strings.Join(opts.NumaNodeIds, ",")
	}

	// Set metadata specific to TPU podslices, TPU slice or TPU devices.
	if isPodslice(opts.Accelerator) && topology != "" {
		if err := addPodsliceOrSliceEnvs(tpuGen, opts.EnableICIResiliency, opts.RequestedChipCount, topologyDims, envs); err != nil {
			return nil, err
		}
	}

	// For single host we can add additional env vars to further reduce
	// configuration required by user.
	if isSingleHost(opts.ChipCount, topologyDims) {
		addSingleHostEnvs(envs)
	}
	return envs, nil
}

func GetEnvName(envName string) (string, error) {
	env := os.Getenv(envName)
	if len(env) == 0 {
		return "", fmt.Errorf("empty %s environment variable", envName)
	}
	return env, nil
}

func ChipCount(chipCount string) (int, error) {
	count, err := strconv.Atoi(chipCount)
	if err != nil {
		return -1, err
	}
	return count, nil
}

// TPU accelerator type from node label should be in format:
// "tpu-<gen>-<device/podslice/slice>".
// We convert it to "<gen>-<# of cores>" for consumption by
// libtpu, frameworks, etc.
func convertAcceleratorType(tpuGen string, topologyDims []int64) (string, error) {
	cores, err := numCores(tpuGen, topologyDims)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d", tpuGen, cores), nil
}

// AcceleratorGen obtains the generation (v3, v4, v4lite, etc.)
// from the accelerator type label.
// accelerator: tpu-v3-device or tpu-v3-slice; return: v3
// accelerator: tpu-v4-podslice; return: v4
// accelerator: tpu-v4-lite-device; return: v4lite
// accelerator: tpu-v5-lite-device; return: v5lite
// accelerator: tpu-v5-lite-podslice; return: v5litepod
// accelerator: tpu-v5p-slice; return: v5p
// accelerator: tpu-v6e-slice; return: v6e
// reference: https://docs.cloud.google.com/kubernetes-engine/docs/concepts/plan-tpus#standard
func AcceleratorGen(accelerator string) (string, error) {
	if acceleratorRegex.MatchString(accelerator) {
		return accelerator, nil
	}
	if !pastAcceleratorRegex.MatchString(accelerator) {
		return "", fmt.Errorf("invalid accelerator type: %v", accelerator)
	}

	// Edge cases that match regex but are not valid accelerator types.
	if accelerator == "tpu-v4-device" || accelerator == "tpu-v4-lite-podslice" {
		return "", fmt.Errorf("no such accelerator type: %s", accelerator)
	}

	// v = v2, v3, v4, v5, v5p, v6e
	v := strings.Split(accelerator, "-")[1]

	// append 'lite' to lite device and lite podslice
	if strings.Contains(accelerator, "lite") {
		v = fmt.Sprintf("%slite", v)
	}

	// append 'pod' to v5 lite podslices
	if strings.HasPrefix(v, "v5") && strings.Contains(accelerator, "podslice") {
		v = fmt.Sprintf("%spod", v)
	}
	if _, exists := validTPUGenerations[v]; !exists {
		return "", fmt.Errorf("invalid TPU generation: %s", v)
	}
	return v, nil
}

// numCores calculates the number of cores based on the topology.
// Lite = 1 core per chip
// Non-lite = 2 cores per chip.
func numCores(tpuGen string, topologyDims []int64) (int, error) {
	// Calculate total chips in the podslice.
	totalChips := calculateTotalChips(topologyDims)

	// lite-device and lite-podslice have 1 core per chip.
	// v6e is also a "lite"
	if strings.Contains(tpuGen, "lite") || strings.Contains(tpuGen, "v6e") {
		return totalChips, nil
	}
	// device and podslice have 2 cores per chip.
	return totalChips * 2, nil
}

func calculateHostBounds(requestedChipCount int, topologyDims []int64) (string, error) {
	if len(topologyDims) != 3 {
		return "", fmt.Errorf("invalid topology dimensions: expected 3, got %d", len(topologyDims))
	}

	// Get chips per dimension from the chipCount.
	trayChipNumPerDim, exists := requestedChipCountToChipsPerDimNumaAligned[requestedChipCount]
	if !exists {
		return "", fmt.Errorf("invalid value for chipCount: %d", requestedChipCount)
	}

	// Calculate host bounds using topology dimensions and chips per dimension.
	var hostBounds []string
	for dim, trayChipNum := range trayChipNumPerDim {
		hostBounds = append(hostBounds, strconv.FormatInt(topologyDims[dim]/trayChipNum, 10))
	}
	return strings.Join(hostBounds, ","), nil
}

func calculateTotalChips(topologyDims []int64) int {
	totalChips := 1
	for _, chips := range topologyDims {
		totalChips *= int(chips)
	}
	return totalChips
}

func getTopologyDims(topology string) ([]int64, error) {
	var topologyDims []int64
	topologyDimStrs := strings.Split(topology, "x")
	for _, s := range topologyDimStrs {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, err
		}
		topologyDims = append(topologyDims, int64(n))
	}

	// Add 3rd dimension of 1 to 2D topologies (e.g. 2x2 -> 2x2x1).
	if len(topologyDims) == 2 {
		topologyDims = append(topologyDims, int64(1))
	}
	if len(topologyDims) != 3 {
		return nil, fmt.Errorf("invalid topology format: %s, must be 2D or 3D", topology)
	}
	return topologyDims, nil
}

func getChipsPerHostBounds(requestedChipCount int) (string, error) {
	chipsPerDim, exists := requestedChipCountToChipsPerDimNumaAligned[requestedChipCount]
	if !exists {
		return "", fmt.Errorf("invalid chip count: %d", requestedChipCount)
	}
	var tmp []string
	for _, chips := range chipsPerDim {
		tmp = append(tmp, strconv.Itoa(int(chips)))
	}
	return strings.Join(tmp, ","), nil
}

// Add podslice or slice Envs.
func addPodsliceOrSliceEnvs(tpuGen, enableICIResiliency string, requestedChipCount int, topologyDims []int64, envs map[string]string) error {
	hostBounds, err := calculateHostBounds(requestedChipCount, topologyDims)
	if err != nil {
		return err
	}
	chipsPerHostBounds, err := getChipsPerHostBounds(requestedChipCount)
	if err != nil {
		return err
	}

	wrapVal, err := wrap(tpuGen, topologyDims)
	if err != nil {
		return err
	}

	// Enable ICI resiliency on for v4 / v5p topologies >= 4x4x4.
	if strings.HasPrefix(tpuGen, "v4") || strings.HasPrefix(tpuGen, "v5p") {
		if cubeOrLarger(topologyDims) {
			envs[ICIResiliencyEnv] = "true"
			if strings.ToLower(enableICIResiliency) == "false" {
				envs[ICIResiliencyEnv] = "false"
			}
		}
	}

	envs["TPU_TOPOLOGY_ALT"] = twist
	envs["ALT"] = twist
	envs["TPU_TOPOLOGY_WRAP"] = wrapVal
	envs["WRAP"] = wrapVal
	envs["HOST_BOUNDS"] = hostBounds
	envs["TPU_HOST_BOUNDS"] = hostBounds
	envs["CHIPS_PER_HOST_BOUNDS"] = chipsPerHostBounds
	envs["TPU_CHIPS_PER_HOST_BOUNDS"] = chipsPerHostBounds
	return nil
}

func addSingleHostEnvs(envs map[string]string) {
	envs["TPU_WORKER_ID"] = "0"
	envs["TPU_WORKER_HOSTNAMES"] = "localhost"
}

func isSingleHost(chipCount int, topologyDims []int64) bool {
	// If multiplication of topology dimensions == chip count on this node,
	// this is a single host.
	return chipCount == calculateTotalChips(topologyDims)
}

func wrap(tpuGen string, topologyDims []int64) (string, error) {
	switch tpuGen {
	case "v3", "v4", "v4lite", "v5p":
		return wrapVersion(topologyDims), nil
	case "v5lite", "v5litepod", "v6e":
		return wrapLitePod(topologyDims), nil
	}
	return "", fmt.Errorf("invalid TPU generation: %s", tpuGen)
}

func wrapVersion(topologyDims []int64) string {
	// v4 does wraparound for v4 cube (4x4x4) or larger.
	if cubeOrLarger(topologyDims) {
		return "true,true,true"
	}
	return "false,false,false"
}

func wrapLitePod(topologyDims []int64) string {
	val := []string{"false", "false", "false"}
	for i, dim := range topologyDims {
		if dim == vlpMaxTopologyDim {
			val[i] = "true"
		}
	}
	return strings.Join(val, ",")
}

func isPodslice(accelerator string) bool {
	return strings.HasSuffix(accelerator, "slice")
}

func cubeOrLarger(topologyDims []int64) bool {
	// v4 cube is 4x4x4.
	for _, dim := range topologyDims {
		if dim < 4 {
			return false
		}
	}
	return true
}

// ApplyNetworkSettings iterates through a predefined list of network settings
// and applies them by writing to the corresponding system file. After writing,
// it reads back the value to verify the update and logs the results.
// It traverses the entire list before returning, even if an error is encountered.
func ApplyNetworkSettings() error {
	return applyNetworkSettings(RootDirectory)
}

// An implementation for ApplyNetworkSettings but taking parent directory for unit test purpose.
func applyNetworkSettings(parentDir string) error {
	var errs []string
	for _, setting := range networkSettings {
		filePath := filepath.Join(parentDir, setting.FilePath)
		err := os.WriteFile(filePath, []byte(setting.Value), 0644)
		if err != nil {
			klog.Errorf("Error writing to %s: %v", filePath, err)
			errs = append(errs, filePath)
			continue
		}
		value, err := os.ReadFile(filePath)
		if err != nil {
			klog.Errorf("Error reading from %s: %v", filePath, err)
			errs = append(errs, filePath)
			continue
		}
		klog.Infof("Current value of %s: %s", filePath, value)
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}
