#!/bin/bash
# Description: KOPS on GCE runbook teardown for in-cluster-storage (ics-2)
# Pinned settings: Project: barni-cnrm-20260529, Cluster: ics2
set -euo pipefail

# Locate and source parameters
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "${SCRIPT_DIR}/params.env" ]; then
  source "${SCRIPT_DIR}/params.env"
else
  echo "Error: params.env not found at ${SCRIPT_DIR}/params.env" >&2
  exit 1
fi

echo "=== Tearing down GCP Project: ${PROJECT} ==="
echo "=== Region: ${REGION}, Zone: ${ZONE} ==="
echo "=== KOPS Cluster Name: ${KOPS_CLUSTER_NAME} ==="
echo "=== KOPS State Store: ${KOPS_STATE_STORE} ==="
echo "=== GAR Repository: ${GAR_REPOSITORY} at ${GAR_LOCATION} ==="

# 1. Delete Kubernetes Resources
echo "Deleting CAS CSI Driver..."
kubectl delete csidriver cas.labs.gke.io --ignore-not-found || true
kubectl delete daemonset cas-node-daemon -n kube-agentfs-system --ignore-not-found || true

echo "Deleting ObjectFS and WAL Buffer..."
if [ -f build/manifests/objectfs.yaml ]; then
  kubectl delete -f build/manifests/objectfs.yaml --ignore-not-found || true
else
  kubectl delete -f k8s/objectfs.yaml --ignore-not-found || true
fi

if [ -f build/manifests/wal.yaml ]; then
  kubectl delete -f build/manifests/wal.yaml --ignore-not-found || true
else
  kubectl delete -f k8s/wal.yaml --ignore-not-found || true
fi

kubectl delete namespace kube-objectfs-system --ignore-not-found || true

echo "Deleting AgentFS..."
if [ -f build/manifests/manifest.yaml ]; then
  kubectl delete -f build/manifests/manifest.yaml --ignore-not-found || true
else
  kubectl delete -f k8s/manifest.yaml --ignore-not-found || true
fi
kubectl delete namespace kube-agentfs-system --ignore-not-found || true

# Clean up local temporary manifests
rm -rf build/manifests

# 2. Destroy KOPS GCE cluster
echo "Checking if KOPS cluster ${KOPS_CLUSTER_NAME} exists to delete..."
if kops get cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" >/dev/null 2>&1; then
  echo "Tearing down KOPS GCE cluster ${KOPS_CLUSTER_NAME}..."
  kops delete cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" --yes
else
  echo "KOPS cluster ${KOPS_CLUSTER_NAME} does not exist."
fi

# 3. Clean up GCS State Bucket and GAR Repository
echo "Checking if KOPS state store bucket ${KOPS_STATE_STORE} exists to delete..."
if gsutil ls -b "${KOPS_STATE_STORE}" >/dev/null 2>&1; then
  echo "Deleting KOPS GCS state store bucket ${KOPS_STATE_STORE}..."
  gsutil rm -r "${KOPS_STATE_STORE}" || true
else
  echo "KOPS state store bucket ${KOPS_STATE_STORE} does not exist."
fi

echo "Checking if GAR repository ${GAR_REPOSITORY} exists to delete..."
if gcloud artifacts repositories describe "${GAR_REPOSITORY}" --location="${GAR_LOCATION}" --project="${PROJECT}" >/dev/null 2>&1; then
  echo "Deleting Artifact Registry Repository ${GAR_REPOSITORY}..."
  gcloud artifacts repositories delete "${GAR_REPOSITORY}" \
    --location="${GAR_LOCATION}" \
    --project="${PROJECT}" \
    --quiet || true
else
  echo "GAR repository ${GAR_REPOSITORY} does not exist."
fi

echo "=== Teardown finished! ==="
