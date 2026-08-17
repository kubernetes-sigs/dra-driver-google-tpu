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
	"testing"

	"github.com/urfave/cli/v2"
)

// TestPodUIDFlag verifies the wiring of the --pod-uid flag used for seamless
// upgrades: its value must reach the parsed flag set from the command line,
// from the POD_UID environment variable (how the DaemonSet passes it via the
// downward API), and default to empty so socket names stay non-suffixed when
// it is unset.
func TestPodUIDFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{
			name: "set via command line",
			args: []string{"--pod-uid", "1234-abcd"},
			want: "1234-abcd",
		},
		{
			name: "set via POD_UID environment variable",
			env:  map[string]string{"POD_UID": "5678-efgh"},
			want: "5678-efgh",
		},
		{
			name: "defaults to empty (rolling update disabled)",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear any ambient POD_UID (e.g. when the test itself runs in a
			// pod that injects it via the downward API) so the default case
			// is deterministic.
			t.Setenv("POD_UID", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			app, parsedFlags := newAppWithFlags()
			// The logging configuration can only be applied once per
			// process; this test only exercises flag parsing.
			app.Before = nil
			app.Action = func(c *cli.Context) error {
				return nil
			}

			args := append([]string{"tpu-dra-kubeletplugin", "--node-name", "test-node"}, tt.args...)
			if err := app.Run(args); err != nil {
				t.Fatalf("app.Run() error = %v", err)
			}
			// Assert on the Destination binding, which is what NewDriver
			// reads (config.flags.podUID), not just the cli context.
			if parsedFlags.podUID != tt.want {
				t.Errorf("flags.podUID = %q, want %q", parsedFlags.podUID, tt.want)
			}
		})
	}
}
