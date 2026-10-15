/*
 * Copyright 2026 The Kubernetes Authors.
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
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
)

// fakeDevDir creates a directory containing the given fake TPU device files.
func fakeDevDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func enumerate(t *testing.T, nodeName, tpuGen string, devNames ...string) AllocatableDevices {
	t.Helper()
	t.Setenv("NODE_NAME", nodeName)
	tm := &tpuManager{
		tpuGen:       tpuGen,
		DevDirectory: fakeDevDir(t, devNames...),
		tpuChipCount: len(devNames),
	}
	devs, err := tm.enumerateAllPossibleTpuDevices()
	if err != nil {
		t.Fatalf("enumerateAllPossibleTpuDevices: %v", err)
	}
	if len(devs) != len(devNames) {
		t.Fatalf("got %d devices, want %d", len(devs), len(devNames))
	}
	return devs
}

func TestDeviceUUIDsUniquePerChip(t *testing.T) {
	// Enumeration recognizes the accel<n> devices outside the fixed vfio path, so
	// the fake node exposes four accel-named chips.
	devs := enumerate(t, "node-a", "v4", "accel0", "accel1", "accel2", "accel3")
	seen := map[string]string{}
	for name, d := range devs {
		if !strings.HasPrefix(d.UUID, "tpu-") || len(d.UUID) != len("tpu-")+36 {
			t.Errorf("device %s: malformed uuid %q", name, d.UUID)
		}
		if prev, dup := seen[d.UUID]; dup {
			t.Errorf("devices %s and %s share uuid %s", prev, name, d.UUID)
		}
		seen[d.UUID] = name
	}
}

func TestDeviceUUIDsStableAcrossRestarts(t *testing.T) {
	names := []string{"accel0", "accel1", "accel2", "accel3"}
	first := enumerate(t, "node-a", "v4", names...)
	second := enumerate(t, "node-a", "v4", names...)
	for name, d := range first {
		if got := second[name].UUID; got != d.UUID {
			t.Errorf("device %s: uuid changed across enumerations: %s -> %s", name, d.UUID, got)
		}
	}
}

func TestDeviceUUIDsDifferAcrossNodes(t *testing.T) {
	names := []string{"accel0", "accel1"}
	a := enumerate(t, "node-a", "v4", names...)
	b := enumerate(t, "node-b", "v4", names...)
	for name, d := range a {
		if b[name].UUID == d.UUID {
			t.Errorf("device %s: same uuid %s on different nodes", name, d.UUID)
		}
	}
}

func TestHashDeterministic(t *testing.T) {
	if got1, got2 := hash("node-a"), hash("node-a"); got1 != got2 {
		t.Error("hash is not deterministic for the same input")
	}
	if hash("node-a") == hash("node-b") {
		t.Error("hash collided for distinct inputs")
	}
}

func TestGenerateUUIDsDeterministic(t *testing.T) {
	tm := &tpuManager{}
	a := tm.generateUUIDs("seed")
	b := tm.generateUUIDs("seed")
	if a != b {
		t.Errorf("generateUUIDs not deterministic: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, "tpu-") {
		t.Errorf("expected tpu- prefix, got %q", a)
	}
	if tm.generateUUIDs("other") == a {
		t.Error("expected distinct seeds to produce distinct UUIDs")
	}
}

func TestGetDeviceTopologyOptional(t *testing.T) {
	with := (&AllocatableDevice{name: "d0", topology: "2x2"}).GetDevice()
	if _, ok := with.Attributes["topology"]; !ok {
		t.Error("expected topology attribute when topology is set")
	}

	without := (&AllocatableDevice{name: "d0"}).GetDevice()
	if _, ok := without.Attributes["topology"]; ok {
		t.Error("expected no topology attribute when topology is empty")
	}
	// Core attributes always present.
	for _, k := range []resourceapi.QualifiedName{"index", "uuid", "tpuGen", "brand", "accelerator", "chipCount"} {
		if _, ok := without.Attributes[k]; !ok {
			t.Errorf("missing expected attribute %q", k)
		}
	}
}

func TestDeviceNodeContainerEdits(t *testing.T) {
	v4 := (&tpuManager{DevDirectory: "/dev", tpuGen: "v4"}).DeviceNodeContainerEdits("accel0")
	if len(v4) != 1 {
		t.Errorf("v4: expected 1 device node, got %d", len(v4))
	}

	v6e := (&tpuManager{DevDirectory: "/dev", tpuGen: "v6e"}).DeviceNodeContainerEdits("0")
	if len(v6e) != 2 {
		t.Errorf("v6e: expected 2 device nodes (incl. %s), got %d", defaultDeviceID, len(v6e))
	}
}

func TestValidateTpuRequest(t *testing.T) {
	tm := &tpuManager{tpuChipCount: 4}
	if err := tm.ValidateTpuRequest([]string{"a", "b", "c", "d"}); err != nil {
		t.Errorf("expected full-chip request to pass: %v", err)
	}
	if err := tm.ValidateTpuRequest([]string{"a", "b"}); err == nil {
		t.Error("expected partial request to be rejected")
	}
}

func TestEnumerateAllPossibleTpuDevices(t *testing.T) {
	dir := t.TempDir()
	// v4-style naming (accelN) is matched outside the vfio directory.
	for _, name := range []string{"accel0", "accel1", "not-a-device"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte{}, 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	tm := &tpuManager{DevDirectory: dir, tpuChipCount: 2, tpuGen: "v4"}

	devices, err := tm.enumerateAllPossibleTpuDevices()
	if err != nil {
		t.Fatalf("enumerateAllPossibleTpuDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Errorf("expected 2 matched devices, got %d", len(devices))
	}

	// Count mismatch is an error.
	tmBad := &tpuManager{DevDirectory: dir, tpuChipCount: 3, tpuGen: "v4"}
	if _, err := tmBad.enumerateAllPossibleTpuDevices(); err == nil {
		t.Error("expected error when discovered count != expected chip count")
	}
}
