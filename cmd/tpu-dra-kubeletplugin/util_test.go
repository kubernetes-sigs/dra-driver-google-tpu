package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGetTopologyDims(t *testing.T) {
	tests := []struct {
		name     string
		topology string
		want     []int64
		wantErr  bool
	}{
		{
			name:     "valid 3D topology",
			topology: "2x2x4",
			want:     []int64{2, 2, 4},
			wantErr:  false,
		},
		{
			name:     "valid 2D topology (padded to 3D)",
			topology: "2x2",
			want:     []int64{2, 2, 1},
			wantErr:  false,
		},
		{
			name:     "invalid 1D topology",
			topology: "2",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "invalid topology non-numeric",
			topology: "2xa",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "invalid 4D topology",
			topology: "2x2x2x2",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "empty topology",
			topology: "",
			want:     nil,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getTopologyDims(tt.topology)
			if (err != nil) != tt.wantErr {
				t.Errorf("getTopologyDims() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("getTopologyDims() len got = %v, want %v", len(got), len(tt.want))
					return
				}
				for i := range got {
					if got[i] != tt.want[i] {
						t.Errorf("getTopologyDims() got[%d] = %v, want %v", i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}

func TestIsValidSubSliceTopology(t *testing.T) {
	tests := []struct {
		name             string
		topology         string
		subSliceTopology string
		want             bool
		wantErr          bool
	}{
		{
			name:             "valid matching 3D subslice",
			topology:         "4x4x4",
			subSliceTopology: "2x2x2",
			want:             true,
			wantErr:          false,
		},
		{
			name:             "valid matching 2D subslice",
			topology:         "4x4",
			subSliceTopology: "2x2",
			want:             true,
			wantErr:          false,
		},
		{
			name:             "equivalent 2D topology and 3D subslice",
			topology:         "2x2",
			subSliceTopology: "2x2x1",
			want:             true,
			wantErr:          false,
		},
		{
			name:             "equivalent 3D topology and 2D subslice",
			topology:         "2x2x1",
			subSliceTopology: "2x2",
			want:             true,
			wantErr:          false,
		},
		{
			name:             "subslice topology larger than topology",
			topology:         "2x2x2",
			subSliceTopology: "4x4x4",
			want:             false,
			wantErr:          true,
		},
		{
			name:             "subslice topology larger than topology after normalization",
			topology:         "4x4",
			subSliceTopology: "2x2x2",
			want:             false,
			wantErr:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsValidSubSliceTopology(tt.topology, tt.subSliceTopology)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsValidSubSliceTopology() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got != tt.want {
					t.Errorf("IsValidSubSliceTopology() got = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestAcceleratorGen(t *testing.T) {
	tests := []struct {
		name        string
		accelerator string
		want        string
		wantErr     bool
	}{
		{
			name:        "valid v3 device",
			accelerator: "tpu-v3-device",
			want:        "v3",
			wantErr:     false,
		},
		{
			name:        "valid v3 slice",
			accelerator: "tpu-v3-slice",
			want:        "v3",
			wantErr:     false,
		},
		{
			name:        "valid v4 podslice",
			accelerator: "tpu-v4-podslice",
			want:        "v4",
			wantErr:     false,
		},
		{
			name:        "valid v4 lite device",
			accelerator: "tpu-v4-lite-device",
			want:        "v4lite",
			wantErr:     false,
		},
		{
			name:        "valid v5 lite device",
			accelerator: "tpu-v5-lite-device",
			want:        "v5lite",
			wantErr:     false,
		},
		{
			name:        "valid v5 lite podslice",
			accelerator: "tpu-v5-lite-podslice",
			want:        "v5litepod",
			wantErr:     false,
		},
		{
			name:        "valid v5p slice",
			accelerator: "tpu-v5p-slice",
			want:        "v5p",
			wantErr:     false,
		},
		{
			name:        "valid v6e slice",
			accelerator: "tpu-v6e-slice",
			want:        "v6e",
			wantErr:     false,
		},
		{
			name:        "invalid accelerator random",
			accelerator: "invalid-tpu",
			want:        "",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AcceleratorGen(tt.accelerator)
			if (err != nil) != tt.wantErr {
				t.Errorf("AcceleratorGen() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got != tt.want {
					t.Errorf("AcceleratorGen() got = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestCalculateHostBounds(t *testing.T) {
	tests := []struct {
		name               string
		requestedChipCount int
		topologyDims       []int64
		want               string
		wantErr            bool
	}{
		{
			name:               "valid bounds 1 chip on 2x2x2",
			requestedChipCount: 1,
			topologyDims:       []int64{2, 2, 2},
			want:               "2,2,2", // 2/1, 2/1, 2/1
			wantErr:            false,
		},
		{
			name:               "valid bounds 2 chips on 2x2x2",
			requestedChipCount: 2,
			topologyDims:       []int64{2, 2, 2},
			want:               "2,1,2", // 2/1, 2/2, 2/1
			wantErr:            false,
		},
		{
			name:               "valid bounds 4 chips on 4x4x4",
			requestedChipCount: 4,
			topologyDims:       []int64{4, 4, 4},
			want:               "2,2,4", // 4/2, 4/2, 4/1
			wantErr:            false,
		},
		{
			name:               "valid bounds 8 chips on 8x8x8",
			requestedChipCount: 8,
			topologyDims:       []int64{8, 8, 8},
			want:               "4,2,8", // 8/2, 8/4, 8/1
			wantErr:            false,
		},
		{
			name:               "invalid chip count",
			requestedChipCount: 3,
			topologyDims:       []int64{4, 4, 4},
			want:               "",
			wantErr:            true,
		},
		{
			name:               "invalid 2D topology",
			requestedChipCount: 4,
			topologyDims:       []int64{4, 4},
			want:               "",
			wantErr:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := calculateHostBounds(tt.requestedChipCount, tt.topologyDims)
			if (err != nil) != tt.wantErr {
				t.Errorf("calculateHostBounds() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got != tt.want {
					t.Errorf("calculateHostBounds() got = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestGetNodeLabelsFromMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/computeMetadata/v1/instance/attributes/kube-labels" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Errorf("missing Metadata-Flavor header")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("cloud.google.com/gke-tpu-accelerator=tpu-v5-lite-device,cloud.google.com/gke-accelerator-count=4,cloud.google.com/gke-tpu-topology=2x2"))
	}))
	defer server.Close()

	t.Setenv("GCE_METADATA_HOST", server.Listener.Addr().String())

	got, err := getNodeLabelsFromMetadata(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"cloud.google.com/gke-tpu-accelerator":   "tpu-v5-lite-device",
		"cloud.google.com/gke-accelerator-count": "4",
		"cloud.google.com/gke-tpu-topology":      "2x2",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("getNodeLabelsFromMetadata() got = %v, want %v", got, want)
	}
}

func TestGetNodeLabelsFromMetadata_TPUEnvFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Errorf("missing Metadata-Flavor header")
		}

		if r.URL.Path == "/computeMetadata/v1/instance/attributes/kube-labels" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if r.URL.Path == "/computeMetadata/v1/instance/attributes/tpu-env" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`ACCELERATOR_TYPE: 'v5litepod-8'
CHIPS_PER_HOST_BOUNDS: '2,4,1'
ENABLE_ICI_RESILIENCY: 'false'
TOPOLOGY: '2x4'
`))
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	t.Setenv("GCE_METADATA_HOST", server.Listener.Addr().String())

	got, err := getNodeLabelsFromMetadata(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"cloud.google.com/gke-tpu-accelerator":    "tpu-v5-lite-podslice",
		"cloud.google.com/gke-accelerator-count":  "8",
		"cloud.google.com/gke-tpu-topology":       "2x4",
		"cloud.google.com/gke-tpu-ici-resiliency": "false",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("getNodeLabelsFromMetadata() got = %v, want %v", got, want)
	}
}

func TestCalculateTotalChips(t *testing.T) {
	tests := []struct {
		name         string
		topologyDims []int64
		want         int
	}{
		{"single chip", []int64{1, 1, 1}, 1},
		{"2x2x2", []int64{2, 2, 2}, 8},
		{"4x4x4", []int64{4, 4, 4}, 64},
		{"2x2x1", []int64{2, 2, 1}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := calculateTotalChips(tt.topologyDims); got != tt.want {
				t.Errorf("calculateTotalChips(%v) = %d, want %d", tt.topologyDims, got, tt.want)
			}
		})
	}
}

func TestNumCores(t *testing.T) {
	tests := []struct {
		name         string
		tpuGen       string
		topologyDims []int64
		want         int
	}{
		{"v4 non-lite has 2 cores per chip", "v4", []int64{2, 2, 2}, 16},
		{"v5p non-lite", "v5p", []int64{2, 2, 1}, 8},
		{"v4lite has 1 core per chip", "v4lite", []int64{2, 2, 2}, 8},
		{"v5lite", "v5lite", []int64{2, 4, 1}, 8},
		{"v5litepod", "v5litepod", []int64{2, 2, 1}, 4},
		{"v6e treated as lite", "v6e", []int64{2, 2, 1}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := numCores(tt.tpuGen, tt.topologyDims)
			if err != nil {
				t.Fatalf("numCores() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("numCores(%q, %v) = %d, want %d", tt.tpuGen, tt.topologyDims, got, tt.want)
			}
		})
	}
}

func TestIsSingleHost(t *testing.T) {
	tests := []struct {
		name         string
		chipCount    int
		topologyDims []int64
		want         bool
	}{
		{"single host 8 on 2x2x2", 8, []int64{2, 2, 2}, true},
		{"single host 4 on 2x2x1", 4, []int64{2, 2, 1}, true},
		{"single chip", 1, []int64{1, 1, 1}, true},
		{"count less than total", 4, []int64{2, 2, 2}, false},
		{"count greater than total", 16, []int64{2, 2, 2}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSingleHost(tt.chipCount, tt.topologyDims); got != tt.want {
				t.Errorf("isSingleHost(%d, %v) = %v, want %v", tt.chipCount, tt.topologyDims, got, tt.want)
			}
		})
	}
}

func TestConvertAcceleratorType(t *testing.T) {
	tests := []struct {
		name         string
		tpuGen       string
		topologyDims []int64
		want         string
	}{
		{"v4 16 cores", "v4", []int64{2, 2, 2}, "v4-16"},
		{"v5p 8 cores", "v5p", []int64{2, 2, 1}, "v5p-8"},
		{"v6e 4 cores", "v6e", []int64{2, 2, 1}, "v6e-4"},
		{"v4lite 8 cores", "v4lite", []int64{2, 2, 2}, "v4lite-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertAcceleratorType(tt.tpuGen, tt.topologyDims)
			if err != nil {
				t.Fatalf("convertAcceleratorType() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("convertAcceleratorType(%q, %v) = %q, want %q", tt.tpuGen, tt.topologyDims, got, tt.want)
			}
		})
	}
}

func TestGetChipsPerHostBounds(t *testing.T) {
	tests := []struct {
		name               string
		requestedChipCount int
		want               string
		wantErr            bool
	}{
		{"1 chip", 1, "1,1,1", false},
		{"2 chips", 2, "1,2,1", false},
		{"4 chips", 4, "2,2,1", false},
		{"8 chips", 8, "2,4,1", false},
		{"invalid 3 chips", 3, "", true},
		{"invalid zero", 0, "", true},
		{"invalid negative", -1, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getChipsPerHostBounds(tt.requestedChipCount)
			if (err != nil) != tt.wantErr {
				t.Errorf("getChipsPerHostBounds() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("getChipsPerHostBounds(%d) = %q, want %q", tt.requestedChipCount, got, tt.want)
			}
		})
	}
}

func TestIsPodslice(t *testing.T) {
	tests := []struct {
		name        string
		accelerator string
		want        bool
	}{
		{"podslice", "tpu-v4-podslice", true},
		{"slice", "tpu-v3-slice", true},
		{"lite podslice", "tpu-v5-lite-podslice", true},
		{"device", "tpu-v3-device", false},
		{"lite device", "tpu-v4-lite-device", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPodslice(tt.accelerator); got != tt.want {
				t.Errorf("isPodslice(%q) = %v, want %v", tt.accelerator, got, tt.want)
			}
		})
	}
}

func TestCubeOrLarger(t *testing.T) {
	tests := []struct {
		name         string
		topologyDims []int64
		want         bool
	}{
		{"exactly cube 4x4x4", []int64{4, 4, 4}, true},
		{"larger than cube", []int64{8, 8, 8}, true},
		{"below cube", []int64{2, 2, 2}, false},
		{"one dim below", []int64{4, 4, 2}, false},
		{"one dim just below", []int64{4, 4, 3}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cubeOrLarger(tt.topologyDims); got != tt.want {
				t.Errorf("cubeOrLarger(%v) = %v, want %v", tt.topologyDims, got, tt.want)
			}
		})
	}
}

func TestWrapVersion(t *testing.T) {
	tests := []struct {
		name         string
		topologyDims []int64
		want         string
	}{
		{"cube wraps", []int64{4, 4, 4}, "true,true,true"},
		{"below cube no wrap", []int64{2, 2, 2}, "false,false,false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wrapVersion(tt.topologyDims); got != tt.want {
				t.Errorf("wrapVersion(%v) = %q, want %q", tt.topologyDims, got, tt.want)
			}
		})
	}
}

func TestWrapLitePod(t *testing.T) {
	tests := []struct {
		name         string
		topologyDims []int64
		want         string
	}{
		{"both dims at max 16x16", []int64{16, 16, 1}, "true,true,false"},
		{"below max 8x8", []int64{8, 8, 1}, "false,false,false"},
		{"one dim at max 8x16", []int64{8, 16, 1}, "false,true,false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wrapLitePod(tt.topologyDims); got != tt.want {
				t.Errorf("wrapLitePod(%v) = %q, want %q", tt.topologyDims, got, tt.want)
			}
		})
	}
}

func TestWrap(t *testing.T) {
	tests := []struct {
		name         string
		tpuGen       string
		topologyDims []int64
		want         string
		wantErr      bool
	}{
		{"v3 routes to wrapVersion", "v3", []int64{4, 8, 1}, "false,false,false", false},
		{"v4 routes to wrapVersion, cube wraps", "v4", []int64{4, 4, 4}, "true,true,true", false},
		{"v4lite routes to wrapVersion", "v4lite", []int64{2, 2, 1}, "false,false,false", false},
		{"v5p routes to wrapVersion, cube wraps", "v5p", []int64{4, 4, 4}, "true,true,true", false},
		{"v5lite routes to wrapLitePod", "v5lite", []int64{2, 4, 1}, "false,false,false", false},
		{"v5litepod routes to wrapLitePod, dim at max wraps", "v5litepod", []int64{8, 16, 1}, "false,true,false", false},
		{"v6e routes to wrapLitePod, dim at max wraps", "v6e", []int64{8, 16, 1}, "false,true,false", false},
		{"unknown generation errors", "v99", []int64{2, 2, 1}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := wrap(tt.tpuGen, tt.topologyDims)
			if (err != nil) != tt.wantErr {
				t.Errorf("wrap() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("wrap(%q, %v) = %q, want %q", tt.tpuGen, tt.topologyDims, got, tt.want)
			}
		})
	}
}

func TestChipCount(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{"valid", "4", 4, false},
		{"valid larger", "8", 8, false},
		{"non-numeric", "abc", -1, true},
		{"empty", "", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ChipCount(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ChipCount() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("ChipCount(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestAddSingleHostEnvs(t *testing.T) {
	envs := map[string]string{}
	addSingleHostEnvs(envs)
	if envs["TPU_WORKER_ID"] != "0" {
		t.Errorf("TPU_WORKER_ID = %q, want %q", envs["TPU_WORKER_ID"], "0")
	}
	if envs["TPU_WORKER_HOSTNAMES"] != "localhost" {
		t.Errorf("TPU_WORKER_HOSTNAMES = %q, want %q", envs["TPU_WORKER_HOSTNAMES"], "localhost")
	}
}

func TestApplyNetworkSettings(t *testing.T) {
	parentDir := t.TempDir()
	// os.WriteFile does not create parent directories, so create them first.
	for _, s := range networkSettings {
		dir := filepath.Dir(filepath.Join(parentDir, s.FilePath))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir %s: %v", dir, err)
		}
	}
	if err := applyNetworkSettings(parentDir); err != nil {
		t.Fatalf("applyNetworkSettings() unexpected error: %v", err)
	}
	for _, s := range networkSettings {
		got, err := os.ReadFile(filepath.Join(parentDir, s.FilePath))
		if err != nil {
			t.Errorf("reading %s: %v", s.FilePath, err)
			continue
		}
		if string(got) != s.Value {
			t.Errorf("%s = %q, want %q", s.FilePath, string(got), s.Value)
		}
	}
}

func TestApplyNetworkSettingsContinuesOnError(t *testing.T) {
	if len(networkSettings) < 3 {
		t.Fatalf("networkSettings has %d entries, want at least 3", len(networkSettings))
	}
	parentDir := t.TempDir()
	for _, s := range networkSettings {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(parentDir, s.FilePath)), 0755); err != nil {
			t.Fatalf("failed to create dir for %s: %v", s.FilePath, err)
		}
	}
	// Make the first two target paths directories so their writes fail.
	failPaths := []string{
		filepath.Join(parentDir, networkSettings[0].FilePath),
		filepath.Join(parentDir, networkSettings[1].FilePath),
	}
	for _, p := range failPaths {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatalf("failed to create failing path %s: %v", p, err)
		}
	}

	err := applyNetworkSettings(parentDir)
	if err == nil {
		t.Fatal("applyNetworkSettings() = nil, want aggregated error")
	}
	if got, want := err.Error(), strings.Join(failPaths, "; "); got != want {
		t.Errorf("applyNetworkSettings() error = %q, want %q", got, want)
	}

	for _, s := range networkSettings[2:] {
		got, readErr := os.ReadFile(filepath.Join(parentDir, s.FilePath))
		if readErr != nil {
			t.Errorf("setting %s was not written after earlier failures: %v", s.FilePath, readErr)
			continue
		}
		if string(got) != s.Value {
			t.Errorf("setting %s = %q, want %q", s.FilePath, string(got), s.Value)
		}
	}
}
