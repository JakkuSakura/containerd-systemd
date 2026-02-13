package task

import (
	"context"
	"errors"
	"testing"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/opencontainers/runtime-spec/specs-go"
)

type fakeRunner struct {
	mainPIDByUnit map[string]uint32
	mainPIDErr    error
}

func (f *fakeRunner) StartUnit(string, []string, string, map[string]string) error { return nil }
func (f *fakeRunner) StopUnit(string) error                                        { return nil }
func (f *fakeRunner) ResetFailed(string) error                                     { return nil }
func (f *fakeRunner) KillUnit(string, uint32) error                                { return nil }
func (f *fakeRunner) MainPID(unit string) (uint32, error) {
	if f.mainPIDErr != nil {
		return 0, f.mainPIDErr
	}
	return f.mainPIDByUnit[unit], nil
}

type fakeShutdownService struct {
	shutdownCalls int
	doneCh        chan struct{}
}

func newFakeShutdownService() *fakeShutdownService {
	return &fakeShutdownService{doneCh: make(chan struct{})}
}

func (f *fakeShutdownService) Shutdown() {
	f.shutdownCalls++
	select {
	case <-f.doneCh:
	default:
		close(f.doneCh)
	}
}

func (f *fakeShutdownService) RegisterCallback(func(context.Context) error) {}
func (f *fakeShutdownService) Done() <-chan struct{}                       { return f.doneCh }
func (f *fakeShutdownService) Err() error                                  { return nil }

func TestCommandFromSpec(t *testing.T) {
	spec := &specs.Spec{Process: &specs.Process{Args: []string{"/bin/sh", "-c", "echo ok"}, Cwd: "/work", Env: []string{"A=1", "B=2"}}}
	cmd, cwd, env, err := commandFromSpec(spec, "/tmp/bundle")
	if err != nil {
		t.Fatalf("commandFromSpec failed: %v", err)
	}
	if len(cmd) != 3 || cmd[0] != "/bin/sh" {
		t.Fatalf("unexpected command: %#v", cmd)
	}
	if cwd != "/work" {
		t.Fatalf("unexpected cwd: %s", cwd)
	}
	if env["A"] != "1" || env["B"] != "2" {
		t.Fatalf("unexpected env: %#v", env)
	}
}

func TestCommandFromSpecWithRootfsUsesChroot(t *testing.T) {
	spec := &specs.Spec{
		Process: &specs.Process{Args: []string{"/pause"}, Cwd: "/", Env: []string{"A=1"}},
		Root:    &specs.Root{Path: "rootfs"},
	}
	cmd, cwd, env, err := commandFromSpec(spec, "/bundle/path")
	if err != nil {
		t.Fatalf("commandFromSpec failed: %v", err)
	}
	if len(cmd) != 3 || cmd[0] != "chroot" || cmd[1] != "/bundle/path/rootfs" || cmd[2] != "/pause" {
		t.Fatalf("unexpected chroot command: %#v", cmd)
	}
	if cwd != "/" {
		t.Fatalf("unexpected cwd: %s", cwd)
	}
	if env["A"] != "1" {
		t.Fatalf("unexpected env: %#v", env)
	}
}

func TestStateReconcilesRunningToStoppedWhenPidMissing(t *testing.T) {
	svc := newService(nil, nil)
	svc.runner = &fakeRunner{mainPIDByUnit: map[string]uint32{"u1": 0}}
	svc.tasks["t1"] = &taskState{
		id:       "t1",
		bundle:   "/tmp/bundle",
		unitName: "u1",
		status:   tasktypes.Status_RUNNING,
		done:     make(chan struct{}),
	}

	state, err := svc.State(nil, &taskapi.StateRequest{ID: "t1"})
	if err != nil {
		t.Fatalf("State failed: %v", err)
	}
	if state.Status != tasktypes.Status_STOPPED {
		t.Fatalf("expected STOPPED, got %v", state.Status)
	}
	if state.ExitedAt == nil {
		t.Fatal("expected ExitedAt to be set")
	}
}

func TestStateKeepsRunningWhenPidPresent(t *testing.T) {
	svc := newService(nil, nil)
	svc.runner = &fakeRunner{mainPIDByUnit: map[string]uint32{"u2": 4321}}
	svc.tasks["t2"] = &taskState{
		id:       "t2",
		bundle:   "/tmp/bundle",
		unitName: "u2",
		status:   tasktypes.Status_RUNNING,
		done:     make(chan struct{}),
	}

	state, err := svc.State(nil, &taskapi.StateRequest{ID: "t2"})
	if err != nil {
		t.Fatalf("State failed: %v", err)
	}
	if state.Status != tasktypes.Status_RUNNING {
		t.Fatalf("expected RUNNING, got %v", state.Status)
	}
	if state.Pid != 4321 {
		t.Fatalf("expected pid 4321, got %d", state.Pid)
	}
}

func TestDeleteLastTaskTriggersShutdown(t *testing.T) {
	shutdownSvc := newFakeShutdownService()
	svc := newService(nil, shutdownSvc)
	svc.runner = &fakeRunner{mainPIDByUnit: map[string]uint32{"u3": 0}}
	svc.tasks["t3"] = &taskState{
		id:       "t3",
		bundle:   "/tmp/bundle",
		unitName: "u3",
		status:   tasktypes.Status_CREATED,
		done:     make(chan struct{}),
	}

	_, err := svc.Delete(nil, &taskapi.DeleteRequest{ID: "t3"})
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if shutdownSvc.shutdownCalls != 1 {
		t.Fatalf("expected one shutdown call, got %d", shutdownSvc.shutdownCalls)
	}
}

func TestUnitNotLoadedErrorsAreClassified(t *testing.T) {
	if !isUnitNotLoadedError(errors.New("Unit abc.service not loaded")) {
		t.Fatal("expected not-loaded message to be recognized")
	}
	if isUnitNotLoadedError(errors.New("permission denied")) {
		t.Fatal("did not expect generic error to be recognized")
	}
}
