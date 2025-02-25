/*
SPDX-License-Identifier: Apache-2.0

Copyright Contributors to the Submariner project.

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
package lighthouse

import (
	"flag"
	"strconv"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/pkg/errors"
	"github.com/submariner-io/lighthouse/pkg/endpointslice"
	"github.com/submariner-io/lighthouse/pkg/gateway"
	"github.com/submariner-io/lighthouse/pkg/service"
	"github.com/submariner-io/lighthouse/pkg/serviceimport"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	masterURL  string
	kubeconfig string
)

// Hook for unit tests.
var buildKubeConfigFunc = clientcmd.BuildConfigFromFlags

// init registers this plugin within the Caddy plugin framework. It uses "example" as the
// name, and couples it to the Action "setup".
func init() {
	caddy.RegisterPlugin(PluginName, caddy.Plugin{
		ServerType: "dns",
		Action:     setupLighthouse,
	})
}

// setup is the function that gets called when the config parser see the token "lighthouse". Setup is responsible
// for parsing any extra options the this plugin may have. The first token this function sees is "lighthouse".
func setupLighthouse(c *caddy.Controller) error {
	log.Debug("In setupLighthouse")

	l, err := lighthouseParse(c)
	if err != nil {
		return plugin.Error(PluginName, err) // nolint:wrapcheck // No need to wrap this.
	}

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		l.Next = next
		return l
	})

	return nil
}

func lighthouseParse(c *caddy.Controller) (*Lighthouse, error) {
	cfg, err := buildKubeConfigFunc(masterURL, kubeconfig)
	if err != nil {
		return nil, errors.Wrap(err, "error building kubeconfig")
	}

	gwController := gateway.NewController()

	err = gwController.Start(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "error starting the Gateway controller")
	}

	siMap := serviceimport.NewMap(gwController.LocalClusterID())
	siController := serviceimport.NewController(siMap)

	err = siController.Start(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "error starting the ServiceImport controller")
	}

	epMap := endpointslice.NewMap()
	epController := endpointslice.NewController(epMap)

	err = epController.Start(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "error starting the EndpointSlice controller")
	}

	svcController := service.NewController(gwController.LocalClusterID())

	err = svcController.Start(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "error starting the Service controller")
	}

	c.OnShutdown(func() error {
		siController.Stop()
		epController.Stop()
		gwController.Stop()
		svcController.Stop()
		return nil
	})

	lh := &Lighthouse{
		TTL: defaultTTL, ServiceImports: siMap, ClusterStatus: gwController, EndpointSlices: epMap,
		EndpointsStatus: epController, LocalServices: svcController,
	}

	// Changed `for` to `if` to satisfy golint:
	//	 SA4004: the surrounding loop is unconditionally terminated (staticcheck)
	if c.Next() {
		lh.Zones = c.RemainingArgs()
		if len(lh.Zones) == 0 {
			lh.Zones = make([]string, len(c.ServerBlockKeys))
			copy(lh.Zones, c.ServerBlockKeys)
		}

		for i, str := range lh.Zones {
			lh.Zones[i] = plugin.Host(str).Normalize()
		}

		for c.NextBlock() {
			switch c.Val() {
			case "fallthrough":
				lh.Fall.SetZonesFromArgs(c.RemainingArgs())
			case "ttl":
				t, err := parseTTL(c)
				if err != nil {
					return nil, err
				}

				lh.TTL = t
			default:
				if c.Val() != "}" {
					return nil, c.Errf("unknown property '%s'", c.Val()) // nolint:wrapcheck // No need to wrap this.
				}
			}
		}
	}

	return lh, nil
}

func parseTTL(c *caddy.Controller) (uint32, error) {
	// Refer: https://github.com/coredns/coredns/blob/master/plugin/kubernetes/setup.go
	args := c.RemainingArgs()
	if len(args) == 0 {
		return 0, c.ArgErr() // nolint:wrapcheck // No need to wrap this.
	}

	t, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, errors.Wrap(err, "error parsing TTL")
	}

	if t < 0 || t > 3600 {
		return 0, c.Errf("ttl must be in range [0, 3600]: %d", t) // nolint:wrapcheck // No need to wrap this.
	}

	return uint32(t), nil
}

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "",
		"The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
}
