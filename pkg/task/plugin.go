package task

import (
	"fmt"

	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.EventPlugin,
			plugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			publisherRaw, err := ic.GetByID(plugins.EventPlugin, "publisher")
			if err != nil {
				return nil, fmt.Errorf("load publisher plugin: %w", err)
			}
			shutdownRaw, err := ic.GetByID(plugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, fmt.Errorf("load shutdown plugin: %w", err)
			}
			publisher := publisherRaw.(shim.Publisher)
			sd := shutdownRaw.(shutdown.Service)
			return newService(publisher, sd), nil
		},
	})
}
