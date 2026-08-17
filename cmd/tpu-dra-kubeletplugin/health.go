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
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

// The readiness endpoint is served over a Unix domain socket that lives in
// the container's own filesystem (not on a host path), so it never collides
// with the socket of another plugin pod on the same node during a rolling
// update, and it needs no TCP port on the host network namespace.
const (
	defaultHealthSocketPath = "/tmp/tpu-dra-kubeletplugin/health.sock"
	readyzPath              = "/readyz"
	healthcheckTimeout      = 5 * time.Second
)

// registrationStatusFunc reports the plugin registration status as last
// reported by the kubelet, or nil if the kubelet has not called
// NotifyRegistrationStatus yet. It is satisfied by kubeletplugin.Helper.
type registrationStatusFunc func() *registerapi.RegistrationStatus

// healthServer answers readiness probes for the plugin. It reports ready only
// once the kubelet has confirmed the registration of this instance, so that
// during a rolling update the DaemonSet controller does not count the new pod
// as available (and remove the old one) before the new instance can actually
// serve DRA calls.
type healthServer struct {
	socketPath string
	status     registrationStatusFunc

	mu     sync.Mutex
	server *http.Server
	wg     sync.WaitGroup
}

func newHealthServer(socketPath string, status registrationStatusFunc) *healthServer {
	return &healthServer{socketPath: socketPath, status: status}
}

// ServeHTTP implements the /readyz endpoint.
func (h *healthServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != readyzPath {
		http.NotFound(w, r)
		return
	}
	if err := h.ready(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// ready returns nil when the kubelet has registered this plugin instance.
func (h *healthServer) ready() error {
	status := h.status()
	switch {
	case status == nil:
		return errors.New("plugin registration not yet confirmed by the kubelet")
	case !status.PluginRegistered:
		return fmt.Errorf("plugin registration rejected by the kubelet: %s", status.Error)
	}
	return nil
}

// Start listens on the health socket and serves probes in the background.
func (h *healthServer) Start() error {
	if err := os.MkdirAll(filepath.Dir(h.socketPath), 0750); err != nil {
		return fmt.Errorf("create health socket directory: %w", err)
	}
	// Remove a socket left behind by a previous container instance, listen
	// would fail otherwise.
	if err := os.Remove(h.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale health socket: %w", err)
	}
	listener, err := net.Listen("unix", h.socketPath)
	if err != nil {
		return fmt.Errorf("listen on health socket %s: %w", h.socketPath, err)
	}

	h.mu.Lock()
	h.server = &http.Server{Handler: h, ReadHeaderTimeout: healthcheckTimeout}
	server := h.server
	h.mu.Unlock()

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		klog.Infof("Serving readiness probes on %s", h.socketPath)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Errorf("health server failed: %v", err)
		}
	}()
	return nil
}

// Stop shuts the health server down and removes its socket.
func (h *healthServer) Stop() {
	h.mu.Lock()
	server := h.server
	h.server = nil
	h.mu.Unlock()
	if server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		klog.Errorf("health server shutdown: %v", err)
	}
	h.wg.Wait()
	_ = os.Remove(h.socketPath)
}

// checkReady queries the readiness endpoint over the given Unix socket. It
// returns nil when the plugin instance is ready and an error describing why
// not otherwise. This is what the container's readiness probe runs.
func checkReady(ctx context.Context, socketPath string) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://plugin"+readyzPath, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("plugin not reachable at %s: %w", socketPath, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// healthcheckCommand is the `healthcheck` subcommand used as the container's
// exec readiness probe: it exits 0 when the running plugin reports ready.
func healthcheckCommand(flags *Flags) *cli.Command {
	return &cli.Command{
		Name:  "healthcheck",
		Usage: "Exit 0 if the running plugin has been registered with the kubelet (used as the readiness probe).",
		Action: func(c *cli.Context) error {
			ctx, cancel := context.WithTimeout(c.Context, healthcheckTimeout)
			defer cancel()
			if err := checkReady(ctx, flags.healthSocketPath); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			fmt.Println("ready")
			return nil
		},
	}
}
