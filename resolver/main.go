package resolver

import (
	"context"
	"time"

	"github.com/safing/portbase/log"
	"github.com/safing/portbase/modules"
	"github.com/safing/portmaster/intel"

	// module dependencies
	_ "github.com/safing/portmaster/core"
)

var (
	module *modules.Module
)

func init() {
	module = modules.Register("resolver", prep, start, nil, "core", "network")
}

func prep() error {
	intel.SetReverseResolver(ResolveIPAndValidate)

	return prepConfig()
}

func start() error {
	// load resolvers from config and environment
	loadResolvers()

	err := module.RegisterEventHook(
		"network",
		"network changed",
		"update nameservers",
		func(_ context.Context, _ interface{}) error {
			loadResolvers()
			log.Debug("intel: reloaded nameservers due to network change")
			return nil
		},
	)
	if err != nil {
		return err
	}

	module.StartServiceWorker(
		"mdns handler",
		5*time.Second,
		listenToMDNS,
	)

	return nil
}