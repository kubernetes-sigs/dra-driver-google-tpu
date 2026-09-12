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
	"testing"

	"github.com/spf13/pflag"
)

func TestNewLoggingConfig(t *testing.T) {
	l := NewLoggingConfig()
	if l == nil {
		t.Fatal("NewLoggingConfig returned nil")
	}
	if l.config == nil {
		t.Error("expected logging configuration to be initialized")
	}
	if len(l.Flags()) == 0 {
		t.Error("expected logging flags to be registered")
	}
}

func TestPflagToCLI(t *testing.T) {
	pf := &pflag.Flag{
		Name:  "log-flush-frequency",
		Usage: "how often to flush",
		Value: newStubValue("5s"),
	}
	f := pflagToCLI(pf, "Logging:")

	if f.Names()[0] != "log-flush-frequency" {
		t.Errorf("name = %q, want log-flush-frequency", f.Names()[0])
	}
	// Dashes become underscores and upper-cased for the env var.
	enver, ok := f.(interface{ GetEnvVars() []string })
	if !ok {
		t.Fatalf("flag %T does not expose GetEnvVars", f)
	}
	env := enver.GetEnvVars()
	if len(env) != 1 || env[0] != "LOG_FLUSH_FREQUENCY" {
		t.Errorf("env vars = %v, want [LOG_FLUSH_FREQUENCY]", env)
	}
}

type stubValue struct{ v string }

func newStubValue(v string) *stubValue  { return &stubValue{v} }
func (s *stubValue) String() string     { return s.v }
func (s *stubValue) Set(x string) error { s.v = x; return nil }
func (s *stubValue) Type() string       { return "string" }
