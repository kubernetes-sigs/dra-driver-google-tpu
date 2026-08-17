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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urfave/cli/v2"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

// TestReadinessFollowsKubeletRegistrationStatus drives the readiness server
// through the states the kubelet registration goes through and checks the
// probe client (what the container's readiness probe runs) against it:
// not ready before the kubelet has reported anything, not ready when the
// kubelet rejected the registration, ready once it confirmed it, and not
// reachable once the server has been stopped.
func TestReadinessFollowsKubeletRegistrationStatus(t *testing.T) {
	// Unix socket paths are length-limited; keep it short.
	socketPath := filepath.Join(shortTempDir(t), "health.sock")
	var status atomic.Pointer[registerapi.RegistrationStatus]
	hs := newHealthServer(socketPath, status.Load)
	if err := hs.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	ctx := context.Background()

	if err := checkReady(ctx, socketPath); err == nil || !strings.Contains(err.Error(), "not yet confirmed") {
		t.Errorf("before registration: checkReady() error = %v, want 'not yet confirmed'", err)
	}

	status.Store(&registerapi.RegistrationStatus{PluginRegistered: false, Error: "kubelet said no"})
	if err := checkReady(ctx, socketPath); err == nil || !strings.Contains(err.Error(), "kubelet said no") {
		t.Errorf("rejected registration: checkReady() error = %v, want the kubelet's error", err)
	}

	status.Store(&registerapi.RegistrationStatus{PluginRegistered: true})
	if err := checkReady(ctx, socketPath); err != nil {
		t.Errorf("confirmed registration: checkReady() error = %v, want nil", err)
	}

	hs.Stop()
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("after Stop() socket still exists (stat error = %v)", err)
	}
	if err := checkReady(ctx, socketPath); err == nil {
		t.Errorf("after Stop(): checkReady() = nil, want an error")
	}
	// Stop is idempotent.
	hs.Stop()
}

// TestHealthServerReplacesStaleSocket makes sure a socket file left behind by
// a previous container instance does not prevent the server from starting.
func TestHealthServerReplacesStaleSocket(t *testing.T) {
	socketPath := filepath.Join(shortTempDir(t), "health.sock")
	if err := os.WriteFile(socketPath, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	hs := newHealthServer(socketPath, func() *registerapi.RegistrationStatus {
		return &registerapi.RegistrationStatus{PluginRegistered: true}
	})
	if err := hs.Start(); err != nil {
		t.Fatalf("Start() with stale socket error = %v", err)
	}
	defer hs.Stop()
	if err := checkReady(context.Background(), socketPath); err != nil {
		t.Errorf("checkReady() error = %v", err)
	}
}

// TestHealthcheckSubcommand runs the `healthcheck` subcommand the way the
// readiness probe does, against a ready and a not-ready plugin. It must not
// require --node-name, which only the plugin itself needs.
func TestHealthcheckSubcommand(t *testing.T) {
	socketPath := filepath.Join(shortTempDir(t), "health.sock")
	var status atomic.Pointer[registerapi.RegistrationStatus]
	hs := newHealthServer(socketPath, status.Load)
	if err := hs.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer hs.Stop()

	run := func() error {
		app := newApp()
		// The logging configuration can only be applied once per process.
		app.Before = nil
		var exitErr error
		app.ExitErrHandler = func(_ *cli.Context, err error) { exitErr = err }
		if err := app.Run([]string{"tpu-dra-kubeletplugin", "--health-socket", socketPath, "healthcheck"}); err != nil {
			return err
		}
		return exitErr
	}

	if err := run(); err == nil {
		t.Errorf("healthcheck against unregistered plugin: error = nil, want non-nil")
	}
	status.Store(&registerapi.RegistrationStatus{PluginRegistered: true})
	if err := run(); err != nil {
		t.Errorf("healthcheck against registered plugin: error = %v, want nil", err)
	}
}

// shortTempDir returns a per-test temporary directory with a short path, as
// Unix domain socket paths are limited to ~100 bytes on some platforms.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tpu-h-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
