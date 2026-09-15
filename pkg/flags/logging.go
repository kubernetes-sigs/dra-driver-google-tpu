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

package flags

import (
	"strings"

	"github.com/spf13/pflag"
	"github.com/urfave/cli/v2"

	logsapi "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register" // for JSON log output support

	"sigs.k8s.io/dra-driver-google-tpu/pkg/featuregates"
)

// LoggingConfig wraps component-base's LoggingConfiguration for the CLI.
type LoggingConfig struct {
	config *logsapi.LoggingConfiguration
}

// NewLoggingConfig creates a new logging configuration with recommended defaults.
func NewLoggingConfig() *LoggingConfig {
	return &LoggingConfig{
		config: logsapi.NewLoggingConfiguration(),
	}
}

// Apply should be called in a cli.App.Before directly after parsing command
// line flags and before running any code which emits log entries.
//
// The logging gates live in the shared registry in pkg/featuregates alongside
// the driver's own gates, so that a single --feature-gates flag (registered by
// FeatureGateConfig) configures both.
func (l *LoggingConfig) Apply() error {
	return logsapi.ValidateAndApply(l.config, featuregates.FeatureGates())
}

// Flags returns the flags for logging configuration (excluding feature gates).
func (l *LoggingConfig) Flags() []cli.Flag {
	var fs pflag.FlagSet
	logsapi.AddFlags(l.config, &fs)

	var flags []cli.Flag
	fs.VisitAll(func(flag *pflag.Flag) {
		flags = append(flags, pflagToCLI(flag, "Logging:"))
	})
	return flags
}

func pflagToCLI(flag *pflag.Flag, category string) cli.Flag {
	return &cli.GenericFlag{
		Name:        flag.Name,
		Category:    category,
		Usage:       flag.Usage,
		Value:       flag.Value,
		Destination: flag.Value,
		EnvVars:     []string{strings.ToUpper(strings.ReplaceAll(flag.Name, "-", "_"))},
	}
}
