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
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	// Canonical, vendor neutral node labels the driver understands. Any
	// platform can set these directly, no cloud provider integration is
	// required.
	AcceleratorLabel      = DriverName + "/accelerator"
	AcceleratorCountLabel = DriverName + "/chip-count"
	TopologyLabel         = DriverName + "/topology"
	ICIResiliency         = DriverName + "/ici-resiliency"

	// GKE advertises the same information under its own label keys.
	gkeAcceleratorLabel      = "cloud.google.com/gke-tpu-accelerator"
	gkeAcceleratorCountLabel = "cloud.google.com/gke-accelerator-count"
	gkeTopologyLabel         = "cloud.google.com/gke-tpu-topology"
	gkeICIResiliencyLabel    = "cloud.google.com/gke-tpu-ici-resiliency"

	twist             = "false"
	vlpMaxTopologyDim = 16
	ICIResiliencyEnv  = "ENABLE_ICI_RESILIENCY"
	RootDirectory     = "/"
	NodeIPEnv         = "NODE_IP"

	KubeLabelsPath = "instance/attributes/kube-labels"
	TpuEnvPath     = "instance/attributes/tpu-env"
)

// errNoTPUDetected is returned when the node the driver runs on does not expose
// any TPU hardware. It is not a fatal condition: the driver is expected to be
// deployed on heterogeneous clusters where only a subset of nodes has TPUs.
var errNoTPUDetected = errors.New("no TPU hardware detected on this node")

var (
	// canonicalLabelAliases maps every canonical TPU label to the equivalent
	// keys used by known platforms. Add an entry here to support a new
	// platform without touching the rest of the driver.
	canonicalLabelAliases = map[string][]string{
		AcceleratorLabel:      {gkeAcceleratorLabel},
		AcceleratorCountLabel: {gkeAcceleratorCountLabel},
		TopologyLabel:         {gkeTopologyLabel},
		ICIResiliency:         {gkeICIResiliencyLabel},
	}

	acceleratorRegex     = regexp.MustCompile(`^tpu\d+[a-z]?$`)
	pastAcceleratorRegex = regexp.MustCompile(`^tpu-v\d+([ep]?-slice)?((?:-lite)?-(device|podslice))?$`)

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
		return -1, fmt.Errorf("invalid TPU chip count %q", chipCount)
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
//
// The generations it can return, and how to add a new one, are defined by
// tpuGenerationFamilies in hardware.go.
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
	if !isValidTPUGeneration(v) {
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
		// The kernel can clamp or normalize a sysctl-style value while still
		// accepting the write, so verify the read-back matches what we wrote.
		// Reads from /proc/sys carry a trailing newline, hence the trim.
		got := strings.TrimSpace(string(value))
		if got != setting.Value {
			klog.Errorf("Value mismatch for %s: wrote %q, read back %q", filePath, setting.Value, got)
			errs = append(errs, filePath)
			continue
		}
		klog.Infof("Current value of %s: %s", filePath, got)
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

// parseKubeLabels parses the comma-separated kube-labels format.
func parseKubeLabels(raw string) map[string]string {
	labels := make(map[string]string)
	pairs := strings.Split(raw, ",")
	for _, pair := range pairs {
		parts := strings.Split(pair, "=")
		if len(parts) == 2 {
			labels[parts[0]] = parts[1]
		}
	}
	return labels
}

// parseTpuEnv parses the multi-line tpu-env key-value format.
func parseTpuEnv(raw string) map[string]string {
	envMap := make(map[string]string)
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			value = strings.Trim(value, "'\"")
			envMap[key] = value
		}
	}
	return envMap
}

// mapTpuEnvToLabels maps parsed tpu-env variables to the canonical TPU labels.
func mapTpuEnvToLabels(envMap map[string]string) map[string]string {
	accelType := envMap["ACCELERATOR_TYPE"]
	if accelType == "" {
		accelType = envMap["TYPE"]
	}
	if accelType == "" {
		return nil
	}

	// Map accelerator to the "tpu-<gen>-<form factor>" naming used by the labels.
	var accelerator string
	accelTypeLower := strings.ToLower(accelType)
	switch {
	case strings.HasPrefix(accelTypeLower, "v5litepod"):
		accelerator = "tpu-v5-lite-podslice"
	case strings.HasPrefix(accelTypeLower, "v5lite"):
		accelerator = "tpu-v5-lite-device"
	case strings.HasPrefix(accelTypeLower, "v5p"):
		accelerator = "tpu-v5p-slice"
	case strings.HasPrefix(accelTypeLower, "v6e"):
		accelerator = "tpu-v6e-slice"
	case strings.HasPrefix(accelTypeLower, "v4"):
		accelerator = "tpu-v4-podslice"
	case strings.HasPrefix(accelTypeLower, "v3"):
		accelerator = "tpu-v3-device"
	default:
		accelerator = accelTypeLower // fallback
	}

	// Determine chip count from CHIPS_PER_HOST_BOUNDS product
	chipCount := "4" // default fallback
	if chipsBounds, exists := envMap["CHIPS_PER_HOST_BOUNDS"]; exists {
		parts := strings.Split(chipsBounds, ",")
		product := 1
		for _, part := range parts {
			val, err := strconv.Atoi(strings.TrimSpace(part))
			if err == nil {
				product *= val
			}
		}
		chipCount = strconv.Itoa(product)
	}

	// Determine topology
	topology := envMap["TOPOLOGY"]
	if topology == "" {
		topology = "2x2" // default fallback
	}

	return map[string]string{
		AcceleratorLabel:      accelerator,
		AcceleratorCountLabel: chipCount,
		TopologyLabel:         topology,
		ICIResiliency:         envMap["ENABLE_ICI_RESILIENCY"],
	}
}

// getNodeLabelsFromMetadata queries GCE MDS for `kube-labels` or falls back to `tpu-env` using the official library.
func getNodeLabelsFromMetadata(ctx context.Context) (map[string]string, error) {
	if !metadata.OnGCE() {
		return nil, fmt.Errorf("not running on a GCE instance")
	}

	var labels map[string]string

	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 15*time.Second, true, func(ctx context.Context) (done bool, err error) {
		// Try to query kube-labels
		kubeLabelsRaw, err := metadata.GetWithContext(ctx, KubeLabelsPath)
		if err == nil {
			parsed := normalizeTPULabels(parseKubeLabels(kubeLabelsRaw))
			if parsed[AcceleratorLabel] != "" {
				labels = parsed
				return true, nil
			}
		}

		// Try to query tpu-env
		tpuEnvRaw, err := metadata.GetWithContext(ctx, TpuEnvPath)
		if err == nil {
			envMap := parseTpuEnv(tpuEnvRaw)
			mapped := mapTpuEnvToLabels(envMap)
			if mapped != nil && mapped[AcceleratorLabel] != "" {
				labels = mapped
				return true, nil
			}
		}

		klog.Infof("could not get kube-labels or tpu-env on GCE ... retrying")
		return false, nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to get node labels from metadata server: %w", err)
	}

	return labels, nil
}

// normalizeTPULabels translates the TPU labels of any known platform to the
// canonical keys used internally by the driver. Labels unrelated to TPUs and
// keys without a value are dropped.
func normalizeTPULabels(labels map[string]string) map[string]string {
	normalized := make(map[string]string, len(canonicalLabelAliases))
	for canonical, aliases := range canonicalLabelAliases {
		keys := append([]string{canonical}, aliases...)
		for _, key := range keys {
			if value := labels[key]; value != "" {
				normalized[canonical] = value
				break
			}
		}
	}
	return normalized
}

// labelsFromConfig builds the canonical TPU labels from the driver command line
// flags. It allows running on hardware the driver cannot introspect on its own.
func labelsFromConfig(f *Flags) (map[string]string, error) {
	labels := normalizeTPULabels(map[string]string{
		AcceleratorLabel:      f.tpuAccelerator,
		AcceleratorCountLabel: f.tpuChipCount,
		TopologyLabel:         f.tpuTopology,
		ICIResiliency:         f.tpuICIResiliency,
	})
	if labels[AcceleratorLabel] == "" {
		return nil, fmt.Errorf("TPU accelerator not configured")
	}
	return labels, nil
}

// labelsFromTPUEnvFile derives the canonical TPU labels from the tpu-env file
// written by the Cloud TPU runtime on the host, e.g. /etc/tpu/tpu-env.
func labelsFromTPUEnvFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, fmt.Errorf("no tpu-env file configured")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	labels := mapTpuEnvToLabels(parseTpuEnv(string(raw)))
	if labels[AcceleratorLabel] == "" {
		return nil, fmt.Errorf("%s does not describe a TPU", path)
	}
	return labels, nil
}

// labelsFromNode derives the canonical TPU labels from the labels the platform
// set on the Node object. The driver only reads them, what it discovers is
// published on the devices of the ResourceSlice.
func labelsFromNode(ctx context.Context, config *Config) (map[string]string, error) {
	if config.coreclient == nil || config.flags.nodeName == "" {
		return nil, fmt.Errorf("no Kubernetes client available")
	}
	node, err := config.coreclient.CoreV1().Nodes().Get(ctx, config.flags.nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return normalizeTPULabels(node.Labels), nil
}

// getTPUNodeLabels describes the TPU of the local node with the canonical
// labels. Sources are tried in order of decreasing precedence so that an
// explicit configuration always wins over auto discovery, and whatever is left
// unknown is completed from hardware, which the caller has already probed so
// that the fail-fast check on a node without a TPU happens once, right at the
// entry point of NewDriver (driver.go).
func getTPUNodeLabels(ctx context.Context, config *Config, hardware *tpuHardware) (map[string]string, error) {
	sources := []struct {
		name string
		get  func(context.Context) (map[string]string, error)
	}{
		{"driver configuration", func(context.Context) (map[string]string, error) {
			return labelsFromConfig(config.flags)
		}},
		{fmt.Sprintf("tpu-env file %q", config.flags.tpuEnvFilePath), func(context.Context) (map[string]string, error) {
			return labelsFromTPUEnvFile(config.flags.tpuEnvFilePath)
		}},
		{"GCE metadata server", getNodeLabelsFromMetadata},
		{"Kubernetes node object", func(ctx context.Context) (map[string]string, error) {
			return labelsFromNode(ctx, config)
		}},
	}

	labels := map[string]string{}
	for _, source := range sources {
		found, err := source.get(ctx)
		if err != nil {
			klog.V(3).Infof("No TPU information from %s: %v", source.name, err)
			continue
		}
		if found[AcceleratorLabel] == "" {
			klog.V(3).Infof("No TPU information from %s", source.name)
			continue
		}
		klog.Infof("Discovered TPU %v from %s", found, source.name)
		labels = found
		break
	}

	completeLabelsFromHardware(labels, hardware)
	if labels[AcceleratorLabel] == "" {
		hint := ""
		if hardware.generation != "" {
			hint = fmt.Sprintf(" of generation %s (read from their PCI id)", hardware.generation)
		}
		return nil, fmt.Errorf("found %d TPU chips%s in %s but no source knows their type, set --tpu-accelerator or the %s node label",
			hardware.chipCount, hint, hardware.devDirectory, AcceleratorLabel)
	}
	return labels, nil
}

// completeLabelsFromHardware fills in the TPU properties that no source knew
// about with the ones observed on the host.
func completeLabelsFromHardware(labels map[string]string, hardware *tpuHardware) {
	chipCount := strconv.Itoa(hardware.chipCount)
	switch labels[AcceleratorCountLabel] {
	case "":
		labels[AcceleratorCountLabel] = chipCount
	case chipCount:
	default:
		klog.Warningf("Node is labeled with %s TPU chips but %s holds %s devices", labels[AcceleratorCountLabel], hardware.devDirectory, chipCount)
	}

	// Cross-check against the generation read from the PCI id (see
	// tpuGenerationFromPCIIDs in hardware.go), when the hardware could tell it.
	if hardware.generation != "" && labels[AcceleratorLabel] != "" {
		if gen, err := AcceleratorGen(labels[AcceleratorLabel]); err == nil && gen != hardware.generation {
			klog.Warningf("Node is labeled with accelerator %q (generation %s) but its PCI id says generation %s", labels[AcceleratorLabel], gen, hardware.generation)
		}
	}

	if labels[TopologyLabel] == "" {
		if topology := singleHostTopology(hardware.chipCount); topology != "" {
			klog.Infof("Assuming the single host topology %s for %d TPU chips", topology, hardware.chipCount)
			labels[TopologyLabel] = topology
		}
	}
}

// singleHostTopology returns the topology of a single host TPU with the given
// number of chips. Nodes that are part of a multi host slice must get their
// topology from the platform, it cannot be observed locally.
func singleHostTopology(chipCount int) string {
	dims, ok := requestedChipCountToChipsPerDimNumaAligned[chipCount]
	if !ok {
		return ""
	}
	// Trailing dimensions of a single chip are implicit, 2x4x1 is written 2x4.
	for len(dims) > 2 && dims[len(dims)-1] == 1 {
		dims = dims[:len(dims)-1]
	}
	parts := make([]string, 0, len(dims))
	for _, dim := range dims {
		parts = append(parts, strconv.FormatInt(dim, 10))
	}
	return strings.Join(parts, "x")
}
