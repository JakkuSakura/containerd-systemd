# containerd-systemd

`containerd-systemd` is a **containerd runtime handler plugin** implemented as a Runtime V2 shim.

It is **not CRI** and does not expose Kubernetes CRI gRPC services.

## Runtime type

Use runtime type:

```text
io.containerd.systemd.v1
```

Expected shim binary name:

```text
containerd-shim-systemd-v1
```

## Build

```bash
go build -o containerd-shim-systemd-v1 ./cmd/containerd-shim-systemd-v1
```

Install binary into containerd's PATH (for example `/usr/local/bin`).

## containerd config example

```toml
version = 3

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.systemd]
  runtime_type = "io.containerd.systemd.v1"
```

## Kubernetes deployment examples

- RuntimeClass manifest: `examples/k8s/runtimeclass-systemd.yaml`
- Smoke pod: `examples/k8s/pod-systemd-smoke.yaml`
- Long-running pod: `examples/k8s/pod-systemd-sleep.yaml`

Apply manually:

```bash
kubectl apply -f examples/k8s/runtimeclass-systemd.yaml
kubectl apply -f examples/k8s/pod-systemd-smoke.yaml
kubectl logs pod/systemd-smoke
```

## Kubernetes runtime test script

Run:

```bash
./scripts/test-k8s-runtimeclass.sh
```

The script validates:
- RuntimeClass exists and is usable.
- A short pod completes successfully.
- A long-running pod starts and is deletable.

The example pods intentionally set:
- `automountServiceAccountToken: false`
- `hostNetwork: true`
- control-plane and not-ready tolerations

This keeps runtime validation usable on single-node/bootstrap clusters where CNI and service account configmaps are not fully ready.

## What this plugin currently does

- Implements a Runtime V2 shim manager (`Start/Stop/Info`) for containerd.
- Registers a `task` TTRPC service plugin for shim task lifecycle.
- Starts tasks as transient `systemd` units via `systemd-run`.
- Supports basic task lifecycle paths: `Create`, `Start`, `State`, `Wait`, `Kill`, `Delete`, `Pids`, `Connect`, `Shutdown`.

## Current limitations

- No exec/resize/close-io/checkpoint/update/stats implementation.
- No cgroup/resource management integration yet.
- Exit/event reconciliation is basic and intended as a foundation.
- Stdio/FIFO plumbing is not fully implemented yet.
