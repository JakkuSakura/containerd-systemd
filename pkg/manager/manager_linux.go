//go:build linux

package manager

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"containerd-systemd/pkg/systemd"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/version"
	"github.com/containerd/errdefs"
)

type Manager struct {
	name string
}

func New(name string) shim.Manager {
	return &Manager{name: name}
}

func (m *Manager) Name() string {
	return m.name
}

func (m *Manager) Start(ctx context.Context, id string, opts shim.StartOpts) (_ shim.BootstrapParams, retErr error) {
	params := shim.BootstrapParams{Version: 3, Protocol: "ttrpc"}

	cmd, err := newCommand(ctx, id, opts.Address, opts.TTRPCAddress, opts.Debug)
	if err != nil {
		return params, err
	}

	socket, err := newShimSocket(ctx, opts.Address, id)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			params.Address = socket.addr
			return params, nil
		}
		return params, err
	}
	defer func() {
		if retErr != nil {
			socket.Close()
		}
	}()
	cmd.ExtraFiles = append(cmd.ExtraFiles, socket.file)

	if err := cmd.Start(); err != nil {
		return params, err
	}
	go cmd.Wait()

	if err := shim.AdjustOOMScore(cmd.Process.Pid); err != nil {
		return params, fmt.Errorf("adjust shim OOM score: %w", err)
	}

	params.Address = socket.addr
	return params, nil
}

func (m *Manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	runner := systemd.NewRunner()
	unit := systemd.UnitName(id)
	pid, _ := runner.MainPID(unit)
	_ = runner.StopUnit(unit)
	_ = runner.ResetFailed(unit)
	return shim.StopStatus{
		ExitedAt:   time.Now(),
		ExitStatus: 0,
		Pid:        int(pid),
	}, nil
}

func (m *Manager) Info(context.Context, io.Reader) (*types.RuntimeInfo, error) {
	return &types.RuntimeInfo{
		Name: m.name,
		Version: &types.RuntimeVersion{
			Version:  version.Version,
			Revision: version.Revision,
		},
	}, nil
}

type shimSocket struct {
	addr string
	ln   io.Closer
	file *os.File
}

func (s *shimSocket) Close() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
	if s.file != nil {
		_ = s.file.Close()
	}
	_ = shim.RemoveSocket(s.addr)
}

func newShimSocket(ctx context.Context, socketPath, id string) (*shimSocket, error) {
	address, err := shim.SocketAddress(ctx, socketPath, id, false)
	if err != nil {
		return nil, err
	}
	ln, err := shim.NewSocket(address)
	if err != nil {
		if !shim.SocketEaddrinuse(err) {
			return nil, fmt.Errorf("create shim socket: %w", err)
		}
		if shim.CanConnect(address) {
			return &shimSocket{addr: address}, errdefs.ErrAlreadyExists
		}
		if rmErr := shim.RemoveSocket(address); rmErr != nil {
			return nil, fmt.Errorf("remove stale socket: %w", rmErr)
		}
		ln, err = shim.NewSocket(address)
		if err != nil {
			return nil, fmt.Errorf("recreate shim socket: %w", err)
		}
	}
	file, err := ln.File()
	if err != nil {
		_ = ln.Close()
		_ = shim.RemoveSocket(address)
		return nil, err
	}
	return &shimSocket{addr: address, ln: ln, file: file}, nil
}

func newCommand(ctx context.Context, id, containerdAddress, ttrpcAddress string, debug bool) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	args := []string{"-namespace", ns, "-id", id, "-address", containerdAddress}
	if debug {
		args = append(args, "-debug")
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = filepath.Clean(cwd)
	cmd.Env = append(
		os.Environ(),
		"GOMAXPROCS=2",
		"TTRPC_ADDRESS="+ttrpcAddress,
		"GRPC_ADDRESS="+containerdAddress,
		"NAMESPACE="+ns,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}
