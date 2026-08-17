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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const TpuConfigKind = "TpuConfig"

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// TpuConfig holds the set of parameters for configuring the TPUs allocated to
// a workload.
type TpuConfig struct {
	metav1.TypeMeta `json:",inline"`

	// Logging tunes libtpu logging in the containers the TPUs are
	// allocated to. When unset, the libtpu defaults apply.
	// +optional
	Logging *LoggingConfig `json:"logging,omitempty"`
}

// LoggingConfig configures libtpu logging for a workload container.
type LoggingConfig struct {
	// Level is the minimum severity of libtpu messages written to the log
	// files under /tmp/tpu_logs (TPU_MIN_LOG_LEVEL):
	// 0=INFO, 1=WARNING, 2=ERROR, 3=FATAL.
	// +optional
	Level *int32 `json:"level,omitempty"`

	// StderrLevel is the minimum severity of libtpu messages mirrored to the
	// container's stderr (TPU_STDERR_LOG_LEVEL). When unset it defaults to
	// Level.
	// +optional
	StderrLevel *int32 `json:"stderrLevel,omitempty"`
}

// DefaultTpuConfig provides the default TPU configuration, which leaves all
// runtime tuning to the libtpu defaults.
func DefaultTpuConfig() *TpuConfig {
	return &TpuConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupName + "/" + Version,
			Kind:       TpuConfigKind,
		},
	}
}

// Normalize updates a TpuConfig with implied default values based on other
// settings.
func (c *TpuConfig) Normalize() error {
	if c == nil {
		return fmt.Errorf("config is 'nil'")
	}
	if c.Logging != nil && c.Logging.StderrLevel == nil && c.Logging.Level != nil {
		level := *c.Logging.Level
		c.Logging.StderrLevel = &level
	}
	return nil
}
