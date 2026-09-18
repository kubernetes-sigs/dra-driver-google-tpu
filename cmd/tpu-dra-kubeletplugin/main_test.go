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

import "testing"

func TestNewApp(t *testing.T) {
	app := newApp()
	if app == nil {
		t.Fatal("newApp returned nil")
	}
	if len(app.Flags) == 0 {
		t.Error("expected the app to register CLI flags")
	}
	names := map[string]bool{}
	for _, f := range app.Flags {
		for _, n := range f.Names() {
			names[n] = true
		}
	}
	// All direct plugin flags, plus one flag each from kubeClientConfig and
	// loggingConfig, to verify newApp composes every flag source together.
	want := []string{
		"node-name",
		"cdi-root",
		"device-classes",
		"kubelet-registrar-directory-path",
		"kubelet-plugins-directory-path",
		"tpu-accelerator",
		"tpu-chip-count",
		"tpu-topology",
		"tpu-ici-resiliency",
		"tpu-env-file",
		"kubeconfig",    // from kubeClientConfig
		"feature-gates", // from loggingConfig
	}
	for _, w := range want {
		if !names[w] {
			t.Errorf("missing expected flag %q", w)
		}
	}
}
