package task

import (
	"containerd-systemd/pkg/systemd"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/errdefs"
	"github.com/containerd/ttrpc"
	"github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type service struct {
	publisher shim.Publisher
	shutdown  shutdown.Service
	runner    systemdRunner

	mu    sync.Mutex
	tasks map[string]*taskState
}

type systemdRunner interface {
	StartUnit(unit string, command []string, workDir string, env map[string]string) error
	StopUnit(unit string) error
	ResetFailed(unit string) error
	KillUnit(unit string, signal uint32) error
	MainPID(unit string) (uint32, error)
}

type taskState struct {
	id       string
	bundle   string
	unitName string
	stdin    string
	stdout   string
	stderr   string
	terminal bool

	pid        uint32
	status     tasktypes.Status
	exitStatus uint32
	exitedAt   *timestamppb.Timestamp
	done       chan struct{}
	doneOnce   sync.Once
}

func newService(publisher shim.Publisher, sd shutdown.Service) *service {
	return &service{
		publisher: publisher,
		shutdown:  sd,
		runner:    systemd.NewRunner(),
		tasks:     map[string]*taskState{},
	}
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	taskapi.RegisterTTRPCTaskService(server, s)
	return nil
}

func (s *service) Create(_ context.Context, req *taskapi.CreateTaskRequest) (*taskapi.CreateTaskResponse, error) {
	if req.GetID() == "" || req.GetBundle() == "" {
		return nil, errdefs.ErrInvalidArgument
	}
	if _, err := loadSpec(req.GetBundle()); err != nil {
		return nil, fmt.Errorf("load OCI spec: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[req.GetID()]; ok {
		return nil, errdefs.ErrAlreadyExists
	}
	s.tasks[req.GetID()] = &taskState{
		id:       req.GetID(),
		bundle:   req.GetBundle(),
		unitName: systemd.UnitName(req.GetID()),
		stdin:    req.GetStdin(),
		stdout:   req.GetStdout(),
		stderr:   req.GetStderr(),
		terminal: req.GetTerminal(),
		status:   tasktypes.Status_CREATED,
		done:     make(chan struct{}),
	}
	return &taskapi.CreateTaskResponse{}, nil
}

func (s *service) Start(_ context.Context, req *taskapi.StartRequest) (*taskapi.StartResponse, error) {
	if req.GetExecID() != "" {
		return nil, errdefs.ErrNotImplemented
	}

	s.mu.Lock()
	task, ok := s.tasks[req.GetID()]
	if !ok {
		s.mu.Unlock()
		return nil, errdefs.ErrNotFound
	}
	if task.status == tasktypes.Status_RUNNING {
		s.mu.Unlock()
		return nil, errdefs.ErrAlreadyExists
	}
	if task.status == tasktypes.Status_STOPPED {
		s.mu.Unlock()
		return nil, errdefs.ErrFailedPrecondition
	}
	s.mu.Unlock()

	spec, err := loadSpec(task.bundle)
	if err != nil {
		return nil, fmt.Errorf("load OCI spec: %w", err)
	}
	command, workDir, envMap, err := commandFromSpec(spec, task.bundle)
	if err != nil {
		return nil, err
	}
	if err := s.runner.StartUnit(task.unitName, command, workDir, envMap); err != nil {
		return nil, err
	}

	pid, err := s.runner.MainPID(task.unitName)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	task.pid = pid
	if pid == 0 {
		s.markStoppedLocked(task, 0)
		return &taskapi.StartResponse{Pid: 0}, nil
	}
	task.status = tasktypes.Status_RUNNING
	return &taskapi.StartResponse{Pid: pid}, nil
}

func (s *service) Delete(_ context.Context, req *taskapi.DeleteRequest) (*taskapi.DeleteResponse, error) {
	if req.GetExecID() != "" {
		return nil, errdefs.ErrNotImplemented
	}

	s.mu.Lock()
	task, ok := s.tasks[req.GetID()]
	if !ok {
		s.mu.Unlock()
		return nil, errdefs.ErrNotFound
	}
	s.reconcileTaskLocked(task)

	if task.status == tasktypes.Status_RUNNING {
		if err := s.runner.StopUnit(task.unitName); err != nil && !isUnitNotLoadedError(err) {
			s.mu.Unlock()
			return nil, err
		}
		if err := s.runner.ResetFailed(task.unitName); err != nil && !isUnitNotLoadedError(err) {
			s.mu.Unlock()
			return nil, err
		}
		s.markStoppedLocked(task, 0)
	}
	if task.status == tasktypes.Status_CREATED {
		s.markStoppedLocked(task, 0)
	}

	resp := &taskapi.DeleteResponse{Pid: task.pid, ExitStatus: task.exitStatus, ExitedAt: task.exitedAt}
	delete(s.tasks, req.GetID())
	shouldShutdown := len(s.tasks) == 0 && s.shutdown != nil
	s.mu.Unlock()
	if shouldShutdown {
		s.shutdown.Shutdown()
	}
	return resp, nil
}

func (s *service) State(_ context.Context, req *taskapi.StateRequest) (*taskapi.StateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[req.GetID()]
	if !ok {
		return nil, errdefs.ErrNotFound
	}
	s.reconcileTaskLocked(task)
	return &taskapi.StateResponse{
		ID:         task.id,
		Bundle:     task.bundle,
		Pid:        task.pid,
		Status:     task.status,
		Stdin:      task.stdin,
		Stdout:     task.stdout,
		Stderr:     task.stderr,
		Terminal:   task.terminal,
		ExitStatus: task.exitStatus,
		ExitedAt:   task.exitedAt,
		ExecID:     "",
	}, nil
}

func (s *service) Wait(ctx context.Context, req *taskapi.WaitRequest) (*taskapi.WaitResponse, error) {
	if req.GetExecID() != "" {
		return nil, errdefs.ErrNotImplemented
	}

	s.mu.Lock()
	task, ok := s.tasks[req.GetID()]
	if !ok {
		s.mu.Unlock()
		return nil, errdefs.ErrNotFound
	}
	s.reconcileTaskLocked(task)
	done := task.done
	if task.status == tasktypes.Status_STOPPED {
		resp := &taskapi.WaitResponse{ExitStatus: task.exitStatus, ExitedAt: task.exitedAt}
		s.mu.Unlock()
		return resp, nil
	}
	s.mu.Unlock()

	select {
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		task = s.tasks[req.GetID()]
		if task == nil {
			return nil, errdefs.ErrNotFound
		}
		return &taskapi.WaitResponse{ExitStatus: task.exitStatus, ExitedAt: task.exitedAt}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *service) Kill(_ context.Context, req *taskapi.KillRequest) (*emptypb.Empty, error) {
	if req.GetExecID() != "" {
		return nil, errdefs.ErrNotImplemented
	}
	s.mu.Lock()
	task, ok := s.tasks[req.GetID()]
	if !ok {
		s.mu.Unlock()
		return nil, errdefs.ErrNotFound
	}
	s.reconcileTaskLocked(task)
	if task.status != tasktypes.Status_RUNNING {
		s.mu.Unlock()
		return &emptypb.Empty{}, nil
	}
	signal := req.GetSignal()
	s.mu.Unlock()

	if err := s.runner.KillUnit(task.unitName, signal); err != nil && !isUnitNotLoadedError(err) {
		return nil, err
	}

	if signal == uint32(syscall.SIGKILL) || signal == uint32(syscall.SIGTERM) {
		s.mu.Lock()
		task, ok = s.tasks[req.GetID()]
		if ok {
			s.markStoppedLocked(task, 128+signal)
		}
		s.mu.Unlock()
	}

	return &emptypb.Empty{}, nil
}

func (s *service) Pids(_ context.Context, req *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[req.GetID()]
	if !ok {
		return nil, errdefs.ErrNotFound
	}
	s.reconcileTaskLocked(task)
	if task.pid == 0 || task.status != tasktypes.Status_RUNNING {
		return &taskapi.PidsResponse{}, nil
	}
	return &taskapi.PidsResponse{Processes: []*tasktypes.ProcessInfo{{Pid: task.pid}}}, nil
}

func (s *service) Connect(_ context.Context, req *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[req.GetID()]
	if !ok {
		return nil, errdefs.ErrNotFound
	}
	s.reconcileTaskLocked(task)
	return &taskapi.ConnectResponse{ShimPid: uint32(os.Getpid()), TaskPid: task.pid, Version: "3"}, nil
}

func (s *service) Shutdown(_ context.Context, req *taskapi.ShutdownRequest) (*emptypb.Empty, error) {
	if s.shutdown != nil {
		s.shutdown.Shutdown()
	}
	return &emptypb.Empty{}, nil
}

func (s *service) Pause(context.Context, *taskapi.PauseRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *service) Resume(context.Context, *taskapi.ResumeRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *service) Checkpoint(context.Context, *taskapi.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *service) Exec(context.Context, *taskapi.ExecProcessRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *service) ResizePty(context.Context, *taskapi.ResizePtyRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *service) CloseIO(context.Context, *taskapi.CloseIORequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *service) Update(context.Context, *taskapi.UpdateTaskRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *service) Stats(context.Context, *taskapi.StatsRequest) (*taskapi.StatsResponse, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *service) reconcileTaskLocked(task *taskState) {
	if task == nil || task.status != tasktypes.Status_RUNNING {
		return
	}
	pid, err := s.runner.MainPID(task.unitName)
	if err != nil {
		if isUnitNotLoadedError(err) {
			s.markStoppedLocked(task, 0)
		}
		return
	}
	if pid == 0 {
		s.markStoppedLocked(task, 0)
		return
	}
	task.pid = pid
}

func (s *service) markStoppedLocked(task *taskState, exitStatus uint32) {
	task.pid = 0
	task.status = tasktypes.Status_STOPPED
	task.exitStatus = exitStatus
	if task.exitedAt == nil {
		task.exitedAt = timestamppb.Now()
	}
	task.doneOnce.Do(func() { close(task.done) })
}

func isUnitNotLoadedError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "not loaded") || strings.Contains(message, "No such file")
}

func loadSpec(bundle string) (*specs.Spec, error) {
	path := filepath.Join(bundle, "config.json")
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var spec specs.Spec
	if err := json.Unmarshal(body, &spec); err != nil {
		return nil, err
	}
	if spec.Process == nil {
		return nil, errors.New("OCI spec process is required")
	}
	if len(spec.Process.Args) == 0 {
		return nil, errors.New("OCI spec process.args is required")
	}
	return &spec, nil
}

func commandFromSpec(spec *specs.Spec, bundle string) ([]string, string, map[string]string, error) {
	if spec == nil || spec.Process == nil {
		return nil, "", nil, errdefs.ErrInvalidArgument
	}
	command := append([]string{}, spec.Process.Args...)
	envMap := make(map[string]string, len(spec.Process.Env))
	for _, entry := range spec.Process.Env {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		envMap[parts[0]] = parts[1]
	}
	workDir := spec.Process.Cwd
	if workDir == "" {
		workDir = "/"
	}
	if spec.Root != nil && spec.Root.Path != "" {
		rootPath := spec.Root.Path
		if !filepath.IsAbs(rootPath) {
			rootPath = filepath.Join(bundle, rootPath)
		}
		command = append([]string{"chroot", rootPath}, command...)
		workDir = "/"
	}
	return command, workDir, envMap, nil
}
