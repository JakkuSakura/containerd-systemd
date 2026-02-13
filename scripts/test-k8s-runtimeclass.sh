#!/usr/bin/env bash
set -euo pipefail

NS="${NS:-containerd-systemd-test}"
TIMEOUT_COMPLETE="${TIMEOUT_COMPLETE:-120s}"
TIMEOUT_RUNNING="${TIMEOUT_RUNNING:-60s}"

if ! command -v kubectl >/dev/null 2>&1; then
  echo "kubectl is required" >&2
  exit 1
fi

echo "[1/6] applying RuntimeClass systemd"
kubectl apply -f examples/k8s/runtimeclass-systemd.yaml

echo "[2/6] preparing namespace ${NS}"
kubectl get ns "${NS}" >/dev/null 2>&1 || kubectl create ns "${NS}" >/dev/null

SMOKE_POD="systemd-smoke-$(date +%s)"
SLEEP_POD="systemd-sleep-$(date +%s)"

cleanup() {
  kubectl -n "${NS}" delete pod "${SMOKE_POD}" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "${NS}" delete pod "${SLEEP_POD}" --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "[3/6] creating smoke pod ${SMOKE_POD}"
cat <<YAML | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${SMOKE_POD}
  namespace: ${NS}
spec:
  automountServiceAccountToken: false
  hostNetwork: true
  restartPolicy: Never
  runtimeClassName: systemd
  tolerations:
    - key: node.kubernetes.io/not-ready
      operator: Exists
      effect: NoSchedule
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
      effect: NoSchedule
    - key: node-role.kubernetes.io/master
      operator: Exists
      effect: NoSchedule
  containers:
    - name: smoke
      image: docker.io/library/busybox:1.36
      command: ["/bin/sh", "-c", "echo systemd-runtime-ok"]
YAML

kubectl -n "${NS}" wait --for=condition=Ready "pod/${SMOKE_POD}" --timeout="${TIMEOUT_RUNNING}" >/dev/null 2>&1 || true
kubectl -n "${NS}" wait --for=condition=PodCompleted "pod/${SMOKE_POD}" --timeout="${TIMEOUT_COMPLETE}"
SMOKE_LOGS="$(kubectl -n "${NS}" logs "${SMOKE_POD}")"
if [[ "${SMOKE_LOGS}" != *"systemd-runtime-ok"* ]]; then
  echo "unexpected smoke pod logs: ${SMOKE_LOGS}" >&2
  exit 1
fi

echo "[4/6] creating sleep pod ${SLEEP_POD}"
cat <<YAML | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${SLEEP_POD}
  namespace: ${NS}
spec:
  automountServiceAccountToken: false
  hostNetwork: true
  restartPolicy: Never
  runtimeClassName: systemd
  tolerations:
    - key: node.kubernetes.io/not-ready
      operator: Exists
      effect: NoSchedule
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
      effect: NoSchedule
    - key: node-role.kubernetes.io/master
      operator: Exists
      effect: NoSchedule
  containers:
    - name: sleeper
      image: docker.io/library/busybox:1.36
      command: ["/bin/sh", "-c", "sleep 300"]
YAML

kubectl -n "${NS}" wait --for=condition=Ready "pod/${SLEEP_POD}" --timeout="${TIMEOUT_RUNNING}"

echo "[5/6] deleting sleep pod and waiting for termination"
kubectl -n "${NS}" delete pod "${SLEEP_POD}" --wait=true

echo "[6/6] checking runtime handler assignment"
SMOKE_HANDLER="$(kubectl -n "${NS}" get pod "${SMOKE_POD}" -o jsonpath='{.spec.runtimeClassName}')"
if [[ "${SMOKE_HANDLER}" != "systemd" ]]; then
  echo "runtimeClassName mismatch: ${SMOKE_HANDLER}" >&2
  exit 1
fi

echo "k8s runtimeclass systemd test passed"
