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
