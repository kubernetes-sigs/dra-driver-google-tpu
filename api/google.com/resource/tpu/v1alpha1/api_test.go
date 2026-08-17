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

package v1alpha1

import (
	"testing"

	"k8s.io/utils/ptr"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name            string
		config          *TpuConfig
		wantErr         bool
		wantStderrLevel *int32
	}{
		{
			name:    "nil config",
			config:  nil,
			wantErr: true,
		},
		{
			name:   "empty config stays empty",
			config: DefaultTpuConfig(),
		},
		{
			name:            "stderrLevel defaults to level",
			config:          &TpuConfig{Logging: &LoggingConfig{Level: ptr.To(int32(2))}},
			wantStderrLevel: ptr.To(int32(2)),
		},
		{
			name:            "explicit stderrLevel is preserved",
			config:          &TpuConfig{Logging: &LoggingConfig{Level: ptr.To(int32(2)), StderrLevel: ptr.To(int32(0))}},
			wantStderrLevel: ptr.To(int32(0)),
		},
		{
			name:   "stderrLevel stays unset without level",
			config: &TpuConfig{Logging: &LoggingConfig{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Normalize()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Normalize() = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize() error = %v, want nil", err)
			}
			var got *int32
			if tt.config.Logging != nil {
				got = tt.config.Logging.StderrLevel
			}
			if (got == nil) != (tt.wantStderrLevel == nil) || (got != nil && *got != *tt.wantStderrLevel) {
				t.Errorf("Normalize() stderrLevel = %v, want %v", got, tt.wantStderrLevel)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *TpuConfig
		wantErr bool
	}{
		{
			name:    "nil config",
			config:  nil,
			wantErr: true,
		},
		{
			name:   "empty config is valid",
			config: DefaultTpuConfig(),
		},
		{
			name:   "valid levels",
			config: &TpuConfig{Logging: &LoggingConfig{Level: ptr.To(int32(0)), StderrLevel: ptr.To(int32(3))}},
		},
		{
			name:    "level above FATAL",
			config:  &TpuConfig{Logging: &LoggingConfig{Level: ptr.To(int32(4))}},
			wantErr: true,
		},
		{
			name:    "negative stderrLevel",
			config:  &TpuConfig{Logging: &LoggingConfig{StderrLevel: ptr.To(int32(-1))}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
