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

	"sigs.k8s.io/dra-driver-google-tpu/pkg/featuregates"
)

// FeatureGateConfig exposes the driver's feature gate registry as a CLI flag.
type FeatureGateConfig struct{}

func NewFeatureGateConfig() *FeatureGateConfig {
	return &FeatureGateConfig{}
}

// Flags returns the --feature-gates flag. It is bound to the shared registry in
// pkg/featuregates, which carries both the driver's gates and the logging gates,
// so this is the only place the flag is defined.
func (f *FeatureGateConfig) Flags() []cli.Flag {
	fg := featuregates.FeatureGates()
	value, ok := fg.(pflag.Value)
	if !ok {
		// featuregate's implementation satisfies pflag.Value; if that ever
		// changes we want to know at startup rather than silently lose the flag.
		panic("feature gate registry does not implement pflag.Value; cannot bind --feature-gates flag")
	}

	return []cli.Flag{
		&cli.GenericFlag{
			Name:     "feature-gates",
			Category: "Feature gates:",
			Usage: "A set of key=value pairs that describe feature gates for alpha/experimental features. " +
				"Options are:\n     " + strings.Join(featuregates.KnownFeatures(), "\n     "),
			Value:       value,
			Destination: value,
			EnvVars:     []string{"FEATURE_GATES"},
		},
	}
}
