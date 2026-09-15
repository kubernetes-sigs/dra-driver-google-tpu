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
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"
	"k8s.io/apimachinery/pkg/util/sets"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	pkgflags "sigs.k8s.io/dra-driver-google-tpu/pkg/flags"
)

const (
	DriverName = "tpu.google.com"

	DriverPluginCheckpointFile = "checkpoint.json"
	allowUnsafeInterruptsFile  = "/sys/module/vfio_iommu_type1/parameters/allow_unsafe_interrupts"

	// noTPURetryPeriod is how often a node without TPU is probed again.
	noTPURetryPeriod = time.Minute
)

type Flags struct {
	kubeClientConfig pkgflags.KubeClientConfig

	nodeName      string
	cdiRoot       string
	deviceClasses sets.Set[string]

	kubeletRegistrarDirectoryPath string
	kubeletPluginsDirectoryPath   string
	consumableShares              string

	// TPU properties of the node. When left empty they are auto discovered.
	tpuAccelerator   string
	tpuChipCount     string
	tpuTopology      string
	tpuICIResiliency string
	tpuEnvFilePath   string
}

type Config struct {
	flags      *Flags
	coreclient coreclientset.Interface
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
	featureGateConfig := pkgflags.NewFeatureGateConfig()
	loggingConfig := pkgflags.NewLoggingConfig()
	flags := &Flags{}
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
			Name:        "tpu-accelerator",
			Usage:       "TPU accelerator type of the node, e.g. tpu-v6e-slice. Overrides auto discovery.",
			Destination: &flags.tpuAccelerator,
			EnvVars:     []string{"TPU_ACCELERATOR"},
		},
		&cli.StringFlag{
			Name:        "tpu-chip-count",
			Usage:       "Number of TPU chips on the node. Overrides auto discovery.",
			Destination: &flags.tpuChipCount,
			EnvVars:     []string{"TPU_CHIP_COUNT"},
		},
		&cli.StringFlag{
			Name:        "tpu-topology",
			Usage:       "TPU topology of the slice the node belongs to, e.g. 2x4. Overrides auto discovery.",
			Destination: &flags.tpuTopology,
			EnvVars:     []string{"TPU_TOPOLOGY"},
		},
		&cli.StringFlag{
			Name:        "tpu-ici-resiliency",
			Usage:       "Whether ICI resiliency is enabled for the slice the node belongs to.",
			Destination: &flags.tpuICIResiliency,
			EnvVars:     []string{"TPU_ICI_RESILIENCY"},
		},
		&cli.StringFlag{
			Name:        "tpu-env-file",
			Usage:       "Absolute path to the tpu-env file written by the Cloud TPU runtime on the host.",
			Value:       "/etc/tpu/tpu-env",
			Destination: &flags.tpuEnvFilePath,
			EnvVars:     []string{"TPU_ENV_FILE"},
		},
		&cli.StringFlag{
			Name: "consumable-shares",
			Usage: "How many ResourceClaims may share the TPU chips on a node: 'disabled', " +
				"'unlimited', or a positive integer. Requires the ConsumableShares feature gate " +
				"and a cluster with DRAConsumableCapacity enabled. Only single-host nodes can " +
				"share; on a multi-host slice this setting is ignored. Note that sharing lets " +
				"claims co-schedule and mount the same chips, it does not let two processes " +
				"drive a chip at the same time.",
			Value:       consumableSharesDisabled,
			Destination: &flags.consumableShares,
			EnvVars:     []string{"CONSUMABLE_SHARES"},
		},
	}
	cliFlags = append(cliFlags, flags.kubeClientConfig.Flags()...)
	cliFlags = append(cliFlags, featureGateConfig.Flags()...)
	cliFlags = append(cliFlags, loggingConfig.Flags()...)

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
			return loggingConfig.Apply()
		},
		Action: func(c *cli.Context) error {
			ctx := c.Context
			if err := validateConsumableShares(flags.consumableShares); err != nil {
				return err
			}
			flags.deviceClasses = sets.New[string](c.StringSlice("device-classes")...)
			clientSets, err := flags.kubeClientConfig.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client: %v", err)
			}

			config := &Config{
				flags:      flags,
				coreclient: clientSets.Core,
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
				return fmt.Errorf("unable to allow unsafe interrupts: %w", err)
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
	for err != nil {
		// The driver is deployed on every node of heterogeneous clusters and a
		// node may get its TPU devices after the plugin started, so a node
		// without TPU must neither crash loop nor give up.
		if !errors.Is(err, errNoTPUDetected) {
			return err
		}
		klog.Infof("No TPU detected on node %q, retrying in %s. Set --tpu-accelerator if this node does have a TPU.", config.flags.nodeName, noTPURetryPeriod)
		select {
		case <-sigc:
			return nil
		case <-ctx.Done():
			return nil
		case <-time.After(noTPURetryPeriod):
		}
		driver, err = NewDriver(ctx, config)
	}

	<-sigc

	return nil
}
