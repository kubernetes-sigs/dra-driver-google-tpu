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
)

// Log levels follow the libtpu convention: 0=INFO, 1=WARNING, 2=ERROR,
// 3=FATAL.
const (
	MinLogLevel = 0
	MaxLogLevel = 3
)

// Validate ensures that LoggingConfig has a valid set of values.
func (l *LoggingConfig) Validate() error {
	if l == nil {
		return nil
	}
	if err := validateLogLevel("level", l.Level); err != nil {
		return err
	}
	return validateLogLevel("stderrLevel", l.StderrLevel)
}

func validateLogLevel(field string, level *int32) error {
	if level == nil {
		return nil
	}
	if *level < MinLogLevel || *level > MaxLogLevel {
		return fmt.Errorf("invalid %s: %d, must be between %d (INFO) and %d (FATAL)", field, *level, MinLogLevel, MaxLogLevel)
	}
	return nil
}

// Validate ensures that TpuConfig has a valid set of values.
func (c *TpuConfig) Validate() error {
	if c == nil {
		return fmt.Errorf("config is 'nil'")
	}
	return c.Logging.Validate()
}
