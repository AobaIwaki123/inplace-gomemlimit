#!/usr/bin/env bash
set -euo pipefail

# In-Place Resize verification script for Kubernetes
POD_NAME="demo-inplace-gomemlimit"
NAMESPACE="${1:-default}"

echo "=== 1. Checking initial memory state of ${POD_NAME} ==="
kubectl get pod "${POD_NAME}" -n "${NAMESPACE}"
kubectl logs "${POD_NAME}" -n "${NAMESPACE}" --tail=3

echo ""
echo "=== 2. Applying In-Place Memory Expansion (512Mi -> 1Gi) ==="
kubectl patch --subresource=resize pod "${POD_NAME}" -n "${NAMESPACE}" \
  --patch '{"spec":{"containers":[{"name":"app","resources":{"limits":{"memory":"1Gi"}}}]}}'

echo ""
echo "=== 3. Waiting for in-place GOMEMLIMIT update... ==="
sleep 4

echo "=== 4. Verifying Pod restarts and updated GOMEMLIMIT ==="
kubectl get pod "${POD_NAME}" -n "${NAMESPACE}"
kubectl logs "${POD_NAME}" -n "${NAMESPACE}" --tail=5
