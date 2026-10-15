package main

import (
	"context"
	"errors"
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
			name:     "zero dimension rejected",
			topology: "2x0x4",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "negative dimension rejected",
			topology: "2x-2",
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

func TestCalculateTotalChips(t *testing.T) {
	tests := []struct {
		name    string
		dims    []int64
		want    int
		wantErr bool
	}{
		{name: "normal topology", dims: []int64{2, 2, 4}, want: 16},
		{name: "oversized product rejected", dims: []int64{1024, 1024, 2}, wantErr: true},
		{name: "single oversized dim does not overflow", dims: []int64{2, 9223372036854775807}, wantErr: true},
		{name: "non-positive dim rejected", dims: []int64{2, 0, 4}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := calculateTotalChips(tt.dims)
			if (err != nil) != tt.wantErr {
				t.Errorf("calculateTotalChips() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("calculateTotalChips() = %v, want %v", got, tt.want)
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
		{name: "valid count", input: "4", want: 4, wantErr: false},
		{name: "non-numeric rejected", input: "x", want: -1, wantErr: true},
		{name: "zero rejected", input: "0", want: -1, wantErr: true},
		{name: "negative rejected", input: "-1", want: -1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ChipCount(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ChipCount() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ChipCount() got = %v, want %v", got, tt.want)
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
		{
			// new-style regex matches but generation is unsupported: #32
			name:        "unsupported new-style future generation",
			accelerator: "tpu7x",
			want:        "",
			wantErr:     true,
		},
		{
			// new-style regex matches shape but is garbage: #32
			name:        "unsupported new-style garbage",
			accelerator: "tpu99z",
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
		"tpu.google.com/accelerator": "tpu-v5-lite-device",
		"tpu.google.com/chip-count":  "4",
		"tpu.google.com/topology":    "2x2",
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
		"tpu.google.com/accelerator":    "tpu-v5-lite-podslice",
		"tpu.google.com/chip-count":     "8",
		"tpu.google.com/topology":       "2x4",
		"tpu.google.com/ici-resiliency": "false",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("getNodeLabelsFromMetadata() got = %v, want %v", got, want)
	}
}

func TestNormalizeTPULabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   map[string]string
	}{
		{
			name: "gke labels",
			labels: map[string]string{
				"cloud.google.com/gke-tpu-accelerator":   "tpu-v6e-slice",
				"cloud.google.com/gke-accelerator-count": "8",
				"cloud.google.com/gke-tpu-topology":      "2x4",
				"kubernetes.io/os":                       "linux",
			},
			want: map[string]string{
				"tpu.google.com/accelerator": "tpu-v6e-slice",
				"tpu.google.com/chip-count":  "8",
				"tpu.google.com/topology":    "2x4",
			},
		},
		{
			name: "canonical labels take precedence",
			labels: map[string]string{
				"tpu.google.com/accelerator":           "tpu-v6e-slice",
				"cloud.google.com/gke-tpu-accelerator": "tpu-v4-podslice",
			},
			want: map[string]string{
				"tpu.google.com/accelerator": "tpu-v6e-slice",
			},
		},
		{
			name:   "no tpu labels",
			labels: map[string]string{"kubernetes.io/os": "linux"},
			want:   map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeTPULabels(tt.labels); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("normalizeTPULabels() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyNetworkSettings(t *testing.T) {
	t.Run("writes succeed and read-back matches", func(t *testing.T) {
		root := t.TempDir()
		for _, s := range networkSettings {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, s.FilePath)), 0755); err != nil {
				t.Fatalf("failed to create dir for %s: %v", s.FilePath, err)
			}
		}

		if err := applyNetworkSettings(root); err != nil {
			t.Fatalf("applyNetworkSettings() returned error, want nil: %v", err)
		}

		for _, s := range networkSettings {
			got, err := os.ReadFile(filepath.Join(root, s.FilePath))
			if err != nil {
				t.Errorf("failed to read %s: %v", s.FilePath, err)
				continue
			}
			if strings.TrimSpace(string(got)) != s.Value {
				t.Errorf("%s = %q, want %q", s.FilePath, strings.TrimSpace(string(got)), s.Value)
			}
		}
	})

	t.Run("write failure is reported as error", func(t *testing.T) {
		// Parent directories do not exist, so every write fails.
		err := applyNetworkSettings(t.TempDir())
		if err == nil {
			t.Fatal("applyNetworkSettings() returned nil, want error when writes fail")
		}
	})
}

func TestIsSingleHostNode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name: "single-host podslice: 4 chips, 2x2x1 topology",
			labels: map[string]string{
				AcceleratorLabel:      "tpu-v4-podslice",
				TopologyLabel:         "2x2x1",
				AcceleratorCountLabel: "4",
			},
			want: true,
		},
		{
			name: "single-host podslice: 8 chips, 2x4 topology",
			labels: map[string]string{
				AcceleratorLabel:      "tpu-v5-lite-podslice",
				TopologyLabel:         "2x4",
				AcceleratorCountLabel: "8",
			},
			want: true,
		},
		{
			name: "multi-host podslice: 4 chips of a 4x4x4 slice",
			labels: map[string]string{
				AcceleratorLabel:      "tpu-v4-podslice",
				TopologyLabel:         "4x4x4",
				AcceleratorCountLabel: "4",
			},
			want: false,
		},
		{
			name: "non-slice accelerators are single-host by definition",
			labels: map[string]string{
				AcceleratorLabel:      "tpu-v5-lite-device",
				AcceleratorCountLabel: "1",
			},
			want: true,
		},
		{
			// getTPUNodeLabels normally fills this in via completeLabelsFromHardware,
			// so reaching here means neither the platform nor the hardware could
			// supply a topology.
			name: "podslice with no topology label is treated as multi-host",
			labels: map[string]string{
				AcceleratorLabel:      "tpu-v4-podslice",
				AcceleratorCountLabel: "4",
			},
			want: false,
		},
		{
			// completeLabelsFromHardware assumes a single-host topology for a chip
			// count it recognizes, trimming trailing unit dimensions (4 chips ->
			// "2x2"). Such a node must come out single-host.
			name: "topology inferred from hardware by completeLabelsFromHardware",
			labels: func() map[string]string {
				labels := map[string]string{AcceleratorLabel: "tpu-v4-podslice"}
				completeLabelsFromHardware(labels, &tpuHardware{chipCount: 4})
				return labels
			}(),
			want: true,
		},
		{
			name: "unparsable topology is treated as multi-host",
			labels: map[string]string{
				AcceleratorLabel:      "tpu-v4-podslice",
				TopologyLabel:         "not-a-topology",
				AcceleratorCountLabel: "4",
			},
			want: false,
		},
		{
			name: "unparsable chip count is treated as multi-host",
			labels: map[string]string{
				AcceleratorLabel:      "tpu-v4-podslice",
				TopologyLabel:         "2x2x1",
				AcceleratorCountLabel: "",
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSingleHostNode(tc.labels); got != tc.want {
				t.Errorf("isSingleHostNode(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

func TestGetTPUNodeLabelsFromSources(t *testing.T) {
	tests := []struct {
		name     string
		sources  []tpuLabelSource
		hardware *tpuHardware
		want     map[string]string
		wantErr  bool
	}{
		{
			name: "first source supplies all labels",
			sources: []tpuLabelSource{
				{
					name: "mock source",
					get: func(context.Context) (map[string]string, error) {
						return map[string]string{
							AcceleratorLabel:      "tpu-v6e-slice",
							AcceleratorCountLabel: "4",
							TopologyLabel:         "2x2",
							ICIResiliency:         "true",
						}, nil
					},
				},
			},
			hardware: &tpuHardware{devDirectory: "/dev", chipCount: 4},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v6e-slice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
				ICIResiliency:         "true",
			},
		},
		{
			name: "first source has error, second source succeeds",
			sources: []tpuLabelSource{
				{
					name: "failing source",
					get: func(context.Context) (map[string]string, error) {
						return nil, errors.New("source unavailable")
					},
				},
				{
					name: "successful source",
					get: func(context.Context) (map[string]string, error) {
						return map[string]string{
							AcceleratorLabel:      "tpu-v6e-slice",
							AcceleratorCountLabel: "4",
							TopologyLabel:         "2x2",
						}, nil
					},
				},
			},
			hardware: &tpuHardware{devDirectory: "/dev", chipCount: 4},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v6e-slice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
			},
		},
		{
			name: "source without accelerator is skipped",
			sources: []tpuLabelSource{
				{
					name: "source without accelerator",
					get: func(context.Context) (map[string]string, error) {
						return map[string]string{
							TopologyLabel: "2x2",
						}, nil
					},
				},
				{
					name: "fallback source",
					get: func(context.Context) (map[string]string, error) {
						return map[string]string{
							AcceleratorLabel:      "tpu-v6e-slice",
							AcceleratorCountLabel: "4",
							TopologyLabel:         "2x2",
						}, nil
					},
				},
			},
			hardware: &tpuHardware{devDirectory: "/dev", chipCount: 4},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v6e-slice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
			},
		},
		{
			name: "hardware fills chip count and single-host topology",
			sources: []tpuLabelSource{
				{
					name: "accelerator only",
					get: func(context.Context) (map[string]string, error) {
						return map[string]string{
							AcceleratorLabel: "tpu-v6e-slice",
						}, nil
					},
				},
			},
			hardware: &tpuHardware{devDirectory: "/dev", chipCount: 4},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v6e-slice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
			},
		},
		{
			name: "no source knows the accelerator",
			sources: []tpuLabelSource{
				{
					name: "empty source",
					get: func(context.Context) (map[string]string, error) {
						return nil, errors.New("not found")
					},
				},
			},
			hardware: &tpuHardware{devDirectory: "/dev", chipCount: 4},
			wantErr:  true,
		},
		{
			name:     "empty sources list",
			sources:  nil,
			hardware: &tpuHardware{devDirectory: "/dev", chipCount: 4},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels, err := getTPUNodeLabelsFromSources(context.Background(), tt.sources, tt.hardware)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("getTPUNodeLabelsFromSources: %v", err)
			}
			if !reflect.DeepEqual(labels, tt.want) {
				t.Errorf("labels = %v, want %v", labels, tt.want)
			}
		})
	}
}

func TestDefaultTPULabelSources(t *testing.T) {
	config := &Config{
		flags: &Flags{
			tpuAccelerator: "tpu-v6e-slice",
			tpuChipCount:   "4",
		},
	}
	sources := defaultTPULabelSources(config)
	if len(sources) != 4 {
		t.Fatalf("defaultTPULabelSources returned %d sources, want 4", len(sources))
	}

	// First source is driver configuration and should resolve with the given flags.
	got, err := sources[0].get(context.Background())
	if err != nil {
		t.Fatalf("driver configuration source error: %v", err)
	}
	if got[AcceleratorLabel] != "tpu-v6e-slice" {
		t.Errorf("AcceleratorLabel = %q, want %q", got[AcceleratorLabel], "tpu-v6e-slice")
	}
}

func TestGetTPUNodeLabelsWithConfig(t *testing.T) {
	config := &Config{
		flags: &Flags{
			tpuAccelerator: "tpu-v6e-slice",
			tpuChipCount:   "4",
			tpuTopology:    "2x2",
		},
	}
	hardware := &tpuHardware{devDirectory: "/dev", chipCount: 4}
	labels, err := getTPUNodeLabels(context.Background(), config, hardware)
	if err != nil {
		t.Fatalf("getTPUNodeLabels() unexpected error: %v", err)
	}
	if labels[AcceleratorLabel] != "tpu-v6e-slice" {
		t.Errorf("AcceleratorLabel = %q, want %q", labels[AcceleratorLabel], "tpu-v6e-slice")
	}
}

func TestLabelsFromNodeNoClient(t *testing.T) {
	// Without a client or node name this must fail closed rather than panic.
	if _, err := labelsFromNode(context.Background(), &Config{flags: &Flags{}}); err == nil {
		t.Error("expected error when no Kubernetes client is available")
	}
}

func TestCubeOrLarger(t *testing.T) {
	tests := []struct {
		name string
		dims []int64
		want bool
	}{
		{name: "all dims >= 4", dims: []int64{4, 4, 4}, want: true},
		{name: "larger than cube", dims: []int64{8, 4, 16}, want: true},
		{name: "one dim below 4", dims: []int64{4, 2, 4}, want: false},
		{name: "all dims below 4", dims: []int64{2, 2, 1}, want: false},
		{name: "empty dims is vacuously true", dims: nil, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cubeOrLarger(tt.dims); got != tt.want {
				t.Errorf("cubeOrLarger(%v) = %v, want %v", tt.dims, got, tt.want)
			}
		})
	}
}

func TestWrap(t *testing.T) {
	tests := []struct {
		name    string
		gen     string
		dims    []int64
		want    string
		wantErr bool
	}{
		{name: "v4 cube wraps around", gen: "v4", dims: []int64{4, 4, 4}, want: "true,true,true"},
		{name: "v5p sub-cube does not wrap", gen: "v5p", dims: []int64{2, 2, 1}, want: "false,false,false"},
		{name: "v6e wraps only at the max dim", gen: "v6e", dims: []int64{vlpMaxTopologyDim, 2, vlpMaxTopologyDim}, want: "true,false,true"},
		{name: "v6e below max does not wrap", gen: "v6e", dims: []int64{2, 2, 2}, want: "false,false,false"},
		{name: "unknown generation errors", gen: "v99", dims: []int64{4, 4, 4}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := wrap(tt.gen, tt.dims)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("wrap(%q) expected error, got nil", tt.gen)
				}
				return
			}
			if err != nil {
				t.Fatalf("wrap(%q) error = %v", tt.gen, err)
			}
			if got != tt.want {
				t.Errorf("wrap(%q, %v) = %q, want %q", tt.gen, tt.dims, got, tt.want)
			}
		})
	}
}

func TestInitEnvs(t *testing.T) {
	tests := []struct {
		name    string
		opts    InitEnvOptions
		nodeIP  string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "single-host v6e slice sets worker and slice envs",
			opts: InitEnvOptions{
				Accelerator:           "tpu-v6e-slice",
				Topology:              "2x2",
				ChipCount:             4,
				AcceleratorCount:      4,
				RequestedChipCount:    4,
				EnableDeviceSpreading: true,
				IsPriviledged:         true,
				VisibleChipIds:        []string{"0", "1", "2", "3"},
				NumaNodeIds:           []string{"0"},
			},
			nodeIP: "10.0.0.1",
			want: map[string]string{
				"TPU_SKIP_MDS_QUERY":          "true",
				"TPU_TOPOLOGY":                "2x2",
				"TPU_ACCELERATOR_TYPE":        "v6e-4",
				"VBAR_CONTROL_SERVICE_URL":    "10.0.0.1:8353",
				"TPU_VISIBLE_CHIPS":           "0,1,2,3",
				"WORKLOAD_NIC_PREFERRED_NUMA": "0",
				"TPU_TOPOLOGY_ALT":            "false",
				"ALT":                         "false",
				"TPU_TOPOLOGY_WRAP":           "false,false,false",
				"WRAP":                        "false,false,false",
				"HOST_BOUNDS":                 "1,1,1",
				"TPU_HOST_BOUNDS":             "1,1,1",
				"CHIPS_PER_HOST_BOUNDS":       "2,2,1",
				"TPU_CHIPS_PER_HOST_BOUNDS":   "2,2,1",
				"TPU_WORKER_ID":               "0",
				"TPU_WORKER_HOSTNAMES":        "localhost",
			},
		},
		{
			name: "multi-host v4 cube enables ICI resiliency and omits single-host worker envs",
			opts: InitEnvOptions{
				Accelerator:         "tpu-v4-podslice",
				Topology:            "4x4x4",
				ChipCount:           4,
				AcceleratorCount:    4,
				RequestedChipCount:  4,
				EnableICIResiliency: "true",
			},
			want: map[string]string{
				"TPU_SKIP_MDS_QUERY":        "true",
				"TPU_TOPOLOGY":              "4x4x4",
				"TPU_ACCELERATOR_TYPE":      "v4-128",
				"ENABLE_ICI_RESILIENCY":     "true",
				"TPU_TOPOLOGY_ALT":          "false",
				"ALT":                       "false",
				"TPU_TOPOLOGY_WRAP":         "true,true,true",
				"WRAP":                      "true,true,true",
				"HOST_BOUNDS":               "2,2,4",
				"TPU_HOST_BOUNDS":           "2,2,4",
				"CHIPS_PER_HOST_BOUNDS":     "2,2,1",
				"TPU_CHIPS_PER_HOST_BOUNDS": "2,2,1",
			},
		},
		{
			name: "invalid accelerator errors",
			opts: InitEnvOptions{
				Accelerator: "invalid-tpu",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(NodeIPEnv, tt.nodeIP)
			got, err := InitEnvs(tt.opts)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("InitEnvs(%+v) expected error, got nil", tt.opts)
				}
				return
			}
			if err != nil {
				t.Fatalf("InitEnvs error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("InitEnvs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConvertAcceleratorType(t *testing.T) {
	tests := []struct {
		name    string
		gen     string
		dims    []int64
		want    string
		wantErr bool
	}{
		{name: "lite gen is 1 core per chip", gen: "v4lite", dims: []int64{2, 2, 1}, want: "v4lite-4"},
		{name: "v6e is treated as lite", gen: "v6e", dims: []int64{2, 2, 1}, want: "v6e-4"},
		{name: "non-lite is 2 cores per chip", gen: "v4", dims: []int64{2, 2, 1}, want: "v4-8"},
		{name: "invalid topology errors", gen: "v4", dims: []int64{0, 2}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertAcceleratorType(tt.gen, tt.dims)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("convertAcceleratorType(%q, %v) expected error", tt.gen, tt.dims)
				}
				return
			}
			if err != nil {
				t.Fatalf("convertAcceleratorType error = %v", err)
			}
			if got != tt.want {
				t.Errorf("convertAcceleratorType(%q, %v) = %q, want %q", tt.gen, tt.dims, got, tt.want)
			}
		})
	}
}

func TestLabelsFromConfig(t *testing.T) {
	tests := []struct {
		name    string
		flags   *Flags
		want    map[string]string
		wantErr bool
	}{
		{
			name: "configured flags return canonical labels",
			flags: &Flags{
				tpuAccelerator:   "tpu-v6e-slice",
				tpuChipCount:     "4",
				tpuTopology:      "2x2",
				tpuICIResiliency: "true",
			},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v6e-slice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
				ICIResiliency:         "true",
			},
		},
		{
			name:    "missing accelerator errors",
			flags:   &Flags{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := labelsFromConfig(tt.flags)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("labelsFromConfig(%+v) expected error, got nil", tt.flags)
				}
				return
			}
			if err != nil {
				t.Fatalf("labelsFromConfig error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("labelsFromConfig = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMapTpuEnvToLabels(t *testing.T) {
	tests := []struct {
		name   string
		envMap map[string]string
		want   map[string]string
	}{
		{
			name: "v6e with explicit bounds and topology",
			envMap: map[string]string{
				"ACCELERATOR_TYPE":      "v6e-16",
				"CHIPS_PER_HOST_BOUNDS": "2,2,1",
				"TOPOLOGY":              "4x4",
				"ENABLE_ICI_RESILIENCY": "true",
			},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v6e-slice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "4x4",
				ICIResiliency:         "true",
			},
		},
		{
			name:   "TYPE fallback and default chip/topology",
			envMap: map[string]string{"TYPE": "v5litepod-8"},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v5-lite-podslice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
				ICIResiliency:         "",
			},
		},
		{
			name:   "v5lite non-pod device",
			envMap: map[string]string{"ACCELERATOR_TYPE": "v5lite-8"},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v5-lite-device",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
				ICIResiliency:         "",
			},
		},
		{
			name:   "v5p slice",
			envMap: map[string]string{"ACCELERATOR_TYPE": "v5p-16"},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v5p-slice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
				ICIResiliency:         "",
			},
		},
		{
			name:   "v4 podslice",
			envMap: map[string]string{"ACCELERATOR_TYPE": "v4-8"},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v4-podslice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
				ICIResiliency:         "",
			},
		},
		{
			name:   "v3 device",
			envMap: map[string]string{"ACCELERATOR_TYPE": "v3-8"},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v3-device",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
				ICIResiliency:         "",
			},
		},
		{
			name:   "unknown type falls back to lowercased value",
			envMap: map[string]string{"ACCELERATOR_TYPE": "V9-Custom"},
			want: map[string]string{
				AcceleratorLabel:      "v9-custom",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "2x2",
				ICIResiliency:         "",
			},
		},
		{
			name:   "no accelerator type returns nil",
			envMap: map[string]string{"TOPOLOGY": "2x2"},
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapTpuEnvToLabels(tt.envMap)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("mapTpuEnvToLabels = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLabelsFromTPUEnvFile(t *testing.T) {
	tests := []struct {
		name        string
		emptyPath   bool
		missingFile bool
		content     string
		want        map[string]string
		wantErr     bool
	}{
		{name: "empty path fails closed", emptyPath: true, wantErr: true},
		{name: "missing file surfaces read error", missingFile: true, wantErr: true},
		{name: "non-TPU file is rejected", content: "FOO: bar\n", wantErr: true},
		{
			name:    "well-formed tpu-env yields canonical labels",
			content: "ACCELERATOR_TYPE: 'v6e-16'\nTOPOLOGY: '4x4'\nCHIPS_PER_HOST_BOUNDS: '2,2,1'\n",
			want: map[string]string{
				AcceleratorLabel:      "tpu-v6e-slice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "4x4",
				ICIResiliency:         "",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			switch {
			case tt.emptyPath:
				path = ""
			case tt.missingFile:
				path = filepath.Join(t.TempDir(), "missing")
			default:
				path = filepath.Join(t.TempDir(), "tpu-env")
				if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}

			labels, err := labelsFromTPUEnvFile(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("labelsFromTPUEnvFile(%q) expected error, got nil", path)
				}
				return
			}
			if err != nil {
				t.Fatalf("labelsFromTPUEnvFile error = %v", err)
			}
			if !reflect.DeepEqual(labels, tt.want) {
				t.Errorf("labelsFromTPUEnvFile = %v, want %v", labels, tt.want)
			}
		})
	}
}
