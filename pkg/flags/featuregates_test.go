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
	"slices"
	"strings"
	"testing"
)

func TestFeatureGateConfigFlags(t *testing.T) {
	cfg := NewFeatureGateConfig()
	cliFlags := cfg.Flags()

	if len(cliFlags) != 1 {
		t.Fatalf("Flags() returned %d flags, want 1", len(cliFlags))
	}

	names := cliFlags[0].Names()
	if !slices.Contains(names, "feature-gates") {
		t.Errorf("flag names = %v, want to contain %q", names, "feature-gates")
	}

	// Usage text must expose known features so operators can discover valid gates.
	usage := cliFlags[0].String()
	if !strings.Contains(usage, "ConsumableShares") {
		t.Errorf("flag usage should mention ConsumableShares, got: %s", usage)
	}
}

func TestLoggingConfigFlagsAndApply(t *testing.T) {
	cfg := NewLoggingConfig()
	if err := cfg.Apply(); err != nil {
		t.Fatalf("LoggingConfig.Apply() failed: %v", err)
	}

	flags := cfg.Flags()
	if len(flags) == 0 {
		t.Error("LoggingConfig.Flags() returned no flags")
	}
}
