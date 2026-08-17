/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	resourcev1beta2 "k8s.io/api/resource/v1beta2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/utils/ptr"
)

func TestBuildDeviceStatuses(t *testing.T) {
	state := tpuDeviceState(2, "tpu0", "tpu1")
	state.allocatable["tpu0"].tpuGen = "v5p"
	state.allocatable["tpu1"].tpuGen = "v5p"

	claim := claimWithResults(
		result(DriverName, "tpu-pool", "tpu0", "tpus"),
		result(DriverName, "tpu-pool", "tpu1", "tpus"),
		result(foreignDriverName, "nic-pool", "nic0", "nics"),
		// A foreign device reusing one of our device names must not pick up
		// this driver's attributes.
		result(foreignDriverName, "nic-pool", "tpu0", "nics"),
	)

	statuses := state.buildDeviceStatuses(claim)
	if len(statuses) != 2 {
		t.Fatalf("buildDeviceStatuses() returned %d statuses, want 2", len(statuses))
	}

	for i, want := range []struct {
		device string
		index  int
	}{{"tpu0", 0}, {"tpu1", 1}} {
		status := statuses[i]
		if status.Driver != DriverName || status.Pool != "tpu-pool" || status.Device != want.device {
			t.Errorf("statuses[%d] = %s/%s/%s, want %s/tpu-pool/%s", i, status.Driver, status.Pool, status.Device, DriverName, want.device)
		}
		if status.Data == nil {
			t.Fatalf("statuses[%d].Data is nil", i)
		}
		var data DeviceStatusData
		if err := json.Unmarshal(status.Data.Raw, &data); err != nil {
			t.Fatalf("statuses[%d].Data is not valid JSON: %v", i, err)
		}
		if data.Type != tpuDeviceStatusType || data.UUID != want.device || data.TPUGen != "v5p" || data.Index != want.index {
			t.Errorf("statuses[%d] data = %+v, want type=tpu uuid=%s tpuGen=v5p index=%d", i, data, want.device, want.index)
		}
	}
}

func TestBuildDeviceStatusesEdgeCases(t *testing.T) {
	t.Run("only foreign results", func(t *testing.T) {
		state := tpuDeviceState(2, "tpu0", "tpu1")
		claim := claimWithResults(result(foreignDriverName, "nic-pool", "nic0", "nics"))
		if statuses := state.buildDeviceStatuses(claim); len(statuses) != 0 {
			t.Errorf("got %d statuses for a claim with only foreign results, want 0", len(statuses))
		}
	})

	t.Run("nil allocation does not panic", func(t *testing.T) {
		state := tpuDeviceState(1, "tpu0")
		claim := claimWithResults()
		claim.Status.Allocation = nil
		if statuses := state.buildDeviceStatuses(claim); statuses != nil {
			t.Errorf("got %v for an unallocated claim, want nil", statuses)
		}
	})

	t.Run("duplicate results collapse to one status per key", func(t *testing.T) {
		// adminAccess requests can allocate the same device twice with no
		// ShareID; the API server rejects duplicate (driver,pool,device,shareID)
		// keys, so only one status may be produced.
		state := tpuDeviceState(1, "tpu0")
		claim := claimWithResults(
			result(DriverName, "tpu-pool", "tpu0", "tpus"),
			result(DriverName, "tpu-pool", "tpu0", "admin"),
		)
		if statuses := state.buildDeviceStatuses(claim); len(statuses) != 1 {
			t.Errorf("got %d statuses for duplicate results, want 1", len(statuses))
		}
	})

	t.Run("share IDs distinguish results for the same device", func(t *testing.T) {
		state := tpuDeviceState(1, "tpu0")
		a := result(DriverName, "tpu-pool", "tpu0", "tpus")
		a.ShareID = ptr.To(types.UID("share-a"))
		b := result(DriverName, "tpu-pool", "tpu0", "tpus")
		b.ShareID = ptr.To(types.UID("share-b"))
		statuses := state.buildDeviceStatuses(claimWithResults(a, b))
		if len(statuses) != 2 {
			t.Fatalf("got %d statuses for two shares, want 2", len(statuses))
		}
		if statuses[0].ShareID == nil || *statuses[0].ShareID != "share-a" || statuses[1].ShareID == nil || *statuses[1].ShareID != "share-b" {
			t.Errorf("share IDs not carried through: %+v", statuses)
		}
	})
}

// fakeStateFor wires a DeviceState to a fake clientset seeded with objs and
// returns the clientset so tests can inspect actions and objects.
func fakeStateFor(t *testing.T, chipCount int, objs ...k8sruntime.Object) (*DeviceState, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(objs...)
	state := tpuDeviceState(chipCount, "tpu0")
	state.deviceStatusEnabled = true
	state.draclient = draclient.New(cs)
	return state, cs
}

func countStatusUpdates(cs *fake.Clientset) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.Matches("update", "resourceclaims") && a.GetSubresource() == "status" {
			n++
		}
	}
	return n
}

var freshTPUStatus = []resourceapi.AllocatedDeviceStatus{{
	Driver: DriverName,
	Pool:   "tpu-pool",
	Device: "tpu0",
	Data:   &k8sruntime.RawExtension{Raw: []byte(`{"type":"tpu","uuid":"tpu0","tpuGen":"v5p","index":0}`)},
}}

// updateDeviceStatus must replace only the entries owned by this driver;
// statuses published by other drivers on the same claim have to survive.
func TestUpdateDeviceStatusMergesWithOtherDrivers(t *testing.T) {
	existing := claimWithResults(result(DriverName, "tpu-pool", "tpu0", "tpus"))
	existing.Status.Devices = []resourceapi.AllocatedDeviceStatus{
		{
			Driver: foreignDriverName,
			Pool:   "nic-pool",
			Device: "nic0",
			Data:   &k8sruntime.RawExtension{Raw: []byte(`{"mac":"aa:bb"}`)},
		},
		{
			// A stale entry from this driver that must be replaced, not
			// duplicated.
			Driver: DriverName,
			Pool:   "tpu-pool",
			Device: "tpu0",
			Data:   &k8sruntime.RawExtension{Raw: []byte(`{"uuid":"stale"}`)},
		},
	}
	state, cs := fakeStateFor(t, 1, existing)

	if err := state.updateDeviceStatus(context.Background(), existing, freshTPUStatus); err != nil {
		t.Fatalf("updateDeviceStatus() error = %v", err)
	}

	updated, err := cs.ResourceV1().ResourceClaims(existing.Namespace).Get(context.Background(), existing.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(updated.Status.Devices) != 2 {
		t.Fatalf("claim has %d device statuses, want 2 (foreign + ours): %+v", len(updated.Status.Devices), updated.Status.Devices)
	}

	byDriver := make(map[string]resourceapi.AllocatedDeviceStatus)
	for _, d := range updated.Status.Devices {
		byDriver[d.Driver] = d
	}
	if foreign, ok := byDriver[foreignDriverName]; !ok || string(foreign.Data.Raw) != `{"mac":"aa:bb"}` {
		t.Errorf("foreign driver status was not preserved: %+v", updated.Status.Devices)
	}
	if ours, ok := byDriver[DriverName]; !ok || string(ours.Data.Raw) != string(freshTPUStatus[0].Data.Raw) {
		t.Errorf("this driver's status was not replaced with fresh data: %+v", updated.Status.Devices)
	}
}

// An idempotent Prepare retry must not issue a status write when the claim
// already carries exactly what we would publish.
func TestUpdateDeviceStatusSkipsNoOpWrite(t *testing.T) {
	existing := claimWithResults(result(DriverName, "tpu-pool", "tpu0", "tpus"))
	existing.Status.Devices = freshTPUStatus
	state, cs := fakeStateFor(t, 1, existing)

	if err := state.updateDeviceStatus(context.Background(), existing, freshTPUStatus); err != nil {
		t.Fatalf("updateDeviceStatus() error = %v", err)
	}
	if n := countStatusUpdates(cs); n != 0 {
		t.Errorf("got %d status updates for an unchanged claim, want 0", n)
	}
}

// If the claim was deleted and recreated under the same name, the identities
// we hold belong to the old object and must not be written to the new one.
func TestUpdateDeviceStatusRefusesOnUIDChange(t *testing.T) {
	prepared := claimWithResults(result(DriverName, "tpu-pool", "tpu0", "tpus"))
	recreated := prepared.DeepCopy()
	recreated.UID = "a-different-uid"
	state, cs := fakeStateFor(t, 1, recreated)

	err := state.updateDeviceStatus(context.Background(), prepared, freshTPUStatus)
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("updateDeviceStatus() error = %v, want a UID-changed error", err)
	}
	if n := countStatusUpdates(cs); n != 0 {
		t.Errorf("got %d status updates after a UID change, want 0", n)
	}
}

// A successful publish is remembered so repeated Prepare calls for the same
// claim skip the API round-trip; Unprepare forgets it again.
func TestPublishDeviceStatusRemembersSuccess(t *testing.T) {
	claim := claimWithResults(result(DriverName, "tpu-pool", "tpu0", "tpus"))
	state, cs := fakeStateFor(t, 1, claim)
	ctx := context.Background()

	state.publishDeviceStatus(ctx, claim, freshTPUStatus)
	state.publishDeviceStatus(ctx, claim, freshTPUStatus)
	if got := len(cs.Actions()); got != 2 { // one Get + one UpdateStatus
		t.Errorf("got %d API actions after two publishes of the same claim, want 2: %v", got, cs.Actions())
	}

	state.statusPublished.Delete(string(claim.UID))
	state.publishDeviceStatus(ctx, claim, freshTPUStatus)
	if got := len(cs.Actions()); got != 3 { // + one Get (no-op write skipped)
		t.Errorf("got %d API actions after forgetting the claim, want 3: %v", got, cs.Actions())
	}
}

// A failed publish must not be remembered as done, so the next Prepare
// retries it.
func TestPublishDeviceStatusRetriesAfterFailure(t *testing.T) {
	claim := claimWithResults(result(DriverName, "tpu-pool", "tpu0", "tpus"))
	state, cs := fakeStateFor(t, 1, claim)
	ctx := context.Background()

	fail := true
	cs.PrependReactor("update", "resourceclaims", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if fail {
			return true, nil, context.DeadlineExceeded
		}
		return false, nil, nil
	})

	state.publishDeviceStatus(ctx, claim, freshTPUStatus)
	if state.statusPublished.Load(string(claim.UID)) {
		t.Fatal("claim recorded as published after a failed write")
	}

	fail = false
	state.publishDeviceStatus(ctx, claim, freshTPUStatus)
	if !state.statusPublished.Load(string(claim.UID)) {
		t.Fatal("claim not recorded as published after a successful retry")
	}
	updated, err := cs.ResourceV1().ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(updated.Status.Devices) != 1 {
		t.Errorf("claim has %d device statuses after retry, want 1", len(updated.Status.Devices))
	}
}

// v1beta2Only makes the fake clientset behave like a 1.32/1.33 API server:
// resource.k8s.io/v1 is not served (NotFound) while v1beta2 is.
func v1beta2Only(cs *fake.Clientset) {
	cs.PrependReactor("*", "resourceclaims", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetResource().Version == "v1" {
			return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), "")
		}
		return false, nil, nil
	})
}

// On a cluster that serves only resource.k8s.io/v1beta2 the converting DRA
// client falls back to that version, where its UpdateStatus panics (vendored
// bug). The driver must detect the negotiated version and refuse with an
// error rather than crash the plugin.
func TestUpdateDeviceStatusRefusesV1beta2(t *testing.T) {
	prepared := claimWithResults(result(DriverName, "tpu-pool", "tpu0", "tpus"))
	beta := &resourcev1beta2.ResourceClaim{ObjectMeta: prepared.ObjectMeta}
	beta.Status.Allocation = &resourcev1beta2.AllocationResult{Devices: resourcev1beta2.DeviceAllocationResult{
		Results: []resourcev1beta2.DeviceRequestAllocationResult{{Driver: DriverName, Pool: "tpu-pool", Device: "tpu0", Request: "tpus"}},
	}}
	state, cs := fakeStateFor(t, 1, beta)
	v1beta2Only(cs)

	err := state.updateDeviceStatus(context.Background(), prepared, freshTPUStatus)
	if err == nil || !strings.Contains(err.Error(), "V1beta2") {
		t.Fatalf("updateDeviceStatus() error = %v, want a V1beta2-not-supported error", err)
	}
	if got := state.draclient.CurrentAPI(); got != "V1beta2" {
		t.Fatalf("draclient negotiated %s, want V1beta2 (test setup did not exercise the fallback)", got)
	}
	if n := countStatusUpdates(cs); n != 0 {
		t.Errorf("got %d status updates, want 0", n)
	}

	// Document why the guard exists: the vendored converting client's
	// UpdateStatus panics on this path. If this test starts failing because
	// no panic occurs, the upstream fix has landed and the guard can go.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("draclient UpdateStatus on V1beta2 no longer panics; the guard in updateDeviceStatus can be removed")
			}
		}()
		_, _ = state.draclient.ResourceClaims(prepared.Namespace).UpdateStatus(context.Background(), prepared, metav1.UpdateOptions{})
	}()
}
