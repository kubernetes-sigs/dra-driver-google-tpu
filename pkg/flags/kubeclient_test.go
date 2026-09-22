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

package flags

import (
	"os"
	"path/filepath"
	"testing"
)

const fakeKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user:
    token: fake-token
`

func TestKubeClientConfigFlags(t *testing.T) {
	k := &KubeClientConfig{}
	flags := k.Flags()

	names := map[string]bool{}
	for _, f := range flags {
		names[f.Names()[0]] = true
	}
	for _, want := range []string{"kubeconfig", "kube-api-qps", "kube-api-burst"} {
		if !names[want] {
			t.Errorf("missing expected flag %q", want)
		}
	}
}

func TestNewClientSetConfigFromKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(fakeKubeconfig), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	k := &KubeClientConfig{KubeConfig: path, KubeAPIQPS: 20, KubeAPIBurst: 30}
	cfg, err := k.NewClientSetConfig()
	if err != nil {
		t.Fatalf("NewClientSetConfig: %v", err)
	}
	if cfg.QPS != 20 {
		t.Errorf("QPS = %v, want 20", cfg.QPS)
	}
	if cfg.Burst != 30 {
		t.Errorf("Burst = %d, want 30", cfg.Burst)
	}
}

func TestNewClientSetConfigInClusterFails(t *testing.T) {
	// With no kubeconfig and no in-cluster service account, this must error
	// rather than silently returning a bad config.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	k := &KubeClientConfig{}
	if _, err := k.NewClientSetConfig(); err == nil {
		t.Error("expected error when running out of cluster with no kubeconfig")
	}
}
