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
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/urfave/cli/v2"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-google-tpu/pkg/flags"
)

const (
	DriverName = "tpu.google.com"

	DriverPluginCheckpointFile = "checkpoint.json"
	allowUnsafeInterruptsFile  = "/sys/module/vfio_iommu_type1/parameters/allow_unsafe_interrupts"
)

type Flags struct {
	kubeClientConfig flags.KubeClientConfig
	loggingConfig    *flags.LoggingConfig

	nodeName      string
	cdiRoot       string
	deviceClasses sets.Set[string]

	kubeletRegistrarDirectoryPath string
	kubeletPluginsDirectoryPath   string

	nodeAllocatableMemoryOverheadPerPod       string
	nodeAllocatableMemoryOverheadPerContainer string
	nodeAllocatableCPUOverheadPerPod          string
	nodeAllocatableCPUOverheadPerContainer    string
}

type Config struct {
	flags      *Flags
	coreclient coreclientset.Interface

	// nodeAllocatableResources is the KEP-5517 overhead published on every
	// device, parsed once from the overhead flags at startup. Nil when
	// nothing is configured or the NodeAllocatableResources gate is off.
	// Immutable after startup.
	nodeAllocatableResources map[corev1.ResourceName]resourceapi.NodeAllocatableResource
}

func (c Config) DriverPluginPath() string {
	return filepath.Join(c.flags.kubeletPluginsDirectoryPath, DriverName)
}

func main() {
	klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(klogFlags)

	if err := newApp().Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	flags := &Flags{
		loggingConfig: flags.NewLoggingConfig(),
	}
	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:        "node-name",
			Usage:       "The name of the node to be worked on.",
			Required:    true,
			Destination: &flags.nodeName,
			EnvVars:     []string{"NODE_NAME"},
		},
		&cli.StringFlag{
			Name:        "cdi-root",
			Usage:       "Absolute path to the directory where CDI files will be generated.",
			Value:       "/etc/cdi",
			Destination: &flags.cdiRoot,
			EnvVars:     []string{"CDI_ROOT"},
		},
		&cli.StringSliceFlag{
			Name:    "device-classes",
			Usage:   "The supported set of DRA device classes",
			Value:   cli.NewStringSlice("tpu"),
			EnvVars: []string{"DEVICE_CLASSES"},
		},
		&cli.StringFlag{
			Name:        "kubelet-registrar-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin registrations.",
			Value:       kubeletplugin.KubeletRegistryDir,
			Destination: &flags.kubeletRegistrarDirectoryPath,
			EnvVars:     []string{"KUBELET_REGISTRAR_DIRECTORY_PATH"},
		},
		&cli.StringFlag{
			Name:        "kubelet-plugins-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin data.",
			Value:       kubeletplugin.KubeletPluginsDir,
			Destination: &flags.kubeletPluginsDirectoryPath,
			EnvVars:     []string{"KUBELET_PLUGINS_DIRECTORY_PATH"},
		},
		&cli.StringFlag{
			Name:        "node-allocatable-memory-overhead-per-pod",
			Usage:       "Node-allocatable memory overhead per pod referencing a TPU (resource quantity, e.g. '100Mi'). Requires the NodeAllocatableResources feature gate.",
			Destination: &flags.nodeAllocatableMemoryOverheadPerPod,
			EnvVars:     []string{"NODE_ALLOCATABLE_MEMORY_OVERHEAD_PER_POD"},
		},
		&cli.StringFlag{
			Name:        "node-allocatable-memory-overhead-per-container",
			Usage:       "Node-allocatable memory overhead per container referencing a TPU (resource quantity, e.g. '10Mi'). Requires the NodeAllocatableResources feature gate.",
			Destination: &flags.nodeAllocatableMemoryOverheadPerContainer,
			EnvVars:     []string{"NODE_ALLOCATABLE_MEMORY_OVERHEAD_PER_CONTAINER"},
		},
		&cli.StringFlag{
			Name:        "node-allocatable-cpu-overhead-per-pod",
			Usage:       "Node-allocatable CPU overhead per pod referencing a TPU (resource quantity, e.g. '100m'). Requires the NodeAllocatableResources feature gate.",
			Destination: &flags.nodeAllocatableCPUOverheadPerPod,
			EnvVars:     []string{"NODE_ALLOCATABLE_CPU_OVERHEAD_PER_POD"},
		},
		&cli.StringFlag{
			Name:        "node-allocatable-cpu-overhead-per-container",
			Usage:       "Node-allocatable CPU overhead per container referencing a TPU (resource quantity, e.g. '10m'). Requires the NodeAllocatableResources feature gate.",
			Destination: &flags.nodeAllocatableCPUOverheadPerContainer,
			EnvVars:     []string{"NODE_ALLOCATABLE_CPU_OVERHEAD_PER_CONTAINER"},
		},
	}
	cliFlags = append(cliFlags, flags.kubeClientConfig.Flags()...)
	cliFlags = append(cliFlags, flags.loggingConfig.Flags()...)

	app := &cli.App{
		Name:            "tpu-dra-kubeletplugin",
		Usage:           "tpu-dra-kubeletplugin implements a DRA driver plugin for Cloud TPU.",
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags:           cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			return flags.loggingConfig.Apply()
		},
		Action: func(c *cli.Context) error {
			ctx := c.Context
			flags.deviceClasses = sets.New[string](c.StringSlice("device-classes")...)
			nodeAllocatableResources, err := nodeAllocatableOverheadsFromFlags(flags)
			if err != nil {
				return err
			}
			clientSets, err := flags.kubeClientConfig.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client: %v", err)
			}

			config := &Config{
				flags:                    flags,
				coreclient:               clientSets.Core,
				nodeAllocatableResources: nodeAllocatableResources,
			}

			return StartPlugin(ctx, config)
		},
	}

	return app
}

func StartPlugin(ctx context.Context, config *Config) error {
	err := os.MkdirAll(config.DriverPluginPath(), 0750)
	if err != nil {
		return err
	}

	info, err := os.Stat(config.flags.cdiRoot)
	switch {
	case err != nil && os.IsNotExist(err):
		err := os.MkdirAll(config.flags.cdiRoot, 0750)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("path for cdi file generation is not a directory: '%v'", err)
	}

	// Only write "Y" to this file location if it exists and is not already
	// set. Otherwise, do nothing. Skipping the write when the value is
	// already "Y" keeps the plugin working in environments where /sys is
	// mounted read-only (for example kind) but the host has the parameter
	// set.
	if current, err := os.ReadFile(allowUnsafeInterruptsFile); err == nil {
		if strings.TrimSpace(string(current)) == "Y" {
			klog.Infof("unsafe interrupts already allowed")
		} else {
			// Permission 0644 = readable by all user groups, but writable by this user only.
			if err := os.WriteFile(allowUnsafeInterruptsFile, []byte("Y"), 0644); err != nil {
				panic(err)
			}
			klog.Infof("successfully allowed unsafe interrupts")
		}
	}

	// setup signal for graceful shutdown
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	// Create a cancellable context for cleanup
	var driver *driver
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		if err := driver.Shutdown(ctx); err != nil {
			klog.Errorf("Unable to cleanly shutdown driver: %v", err)
		}
		cancel()
	}()

	driver, err = NewDriver(
		ctx,
		config,
	)
	if err != nil {
		return err
	}

	<-sigc

	return nil
}
