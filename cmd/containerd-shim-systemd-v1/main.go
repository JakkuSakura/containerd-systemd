//go:build linux

package main

import (
	"context"

	"containerd-systemd/pkg/manager"
	_ "containerd-systemd/pkg/task"
	"github.com/containerd/containerd/v2/pkg/shim"
)

func main() {
	shim.Run(context.Background(), manager.New("io.containerd.systemd.v1"))
}
