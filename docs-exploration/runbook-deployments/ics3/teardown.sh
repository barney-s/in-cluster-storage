#!/bin/bash
# Description: Automated KOPS on GCE runbook teardown for in-cluster-storage
# Pinned settings: Project: barni-cnrm-20260529, Cluster: ics3
set -euo pipefail

# Get the directory of this script to source params.env correctly
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/params.env"

echo "=== Tearing down KOPS on GCE Project: ${GCP_PROJECT} ==="
echo "=== Region: ${GCP_REGION}, Zone: ${GCP_ZONE} ==="
echo "=== Cluster: ${CLUSTER} ==="
echo "=== KOPS Cluster Name: ${KOPS_CLUSTER_NAME} ==="
echo "=== KOPS State Store: ${KOPS_STATE_STORE} ==="
echo "=== GAR Repository: ${GAR_REPOSITORY} at ${GAR_LOCATION} ==="

# Check if kubectl is configured and connected to a cluster
if kubectl cluster-info &>/dev/null; then
  # 1. Delete CAS
  echo "Deleting CAS..."
  kubectl delete csidriver cas.labs.gke.io --ignore-not-found || true
  kubectl delete daemonset cas-node-daemon -n kube-agentfs-system --ignore-not-found || true

  # 2. Delete ObjectFS and WAL Buffer
  echo "Deleting ObjectFS and WAL..."
  if [ -f build/manifests/objectfs.yaml ]; then
    kubectl delete -f build/manifests/objectfs.yaml --ignore-not-found || true
  elif [ -f k8s/objectfs.yaml ]; then
    kubectl delete -f k8s/objectfs.yaml --ignore-not-found || true
  fi

  if [ -f build/manifests/wal.yaml ]; then
    kubectl delete -f build/manifests/wal.yaml --ignore-not-found || true
  elif [ -f k8s/wal.yaml ]; then
    kubectl delete -f k8s/wal.yaml --ignore-not-found || true
  fi

  kubectl delete namespace kube-objectfs-system --ignore-not-found || true

  # 3. Delete AgentFS
  echo "Deleting AgentFS..."
  if [ -f build/manifests/manifest.yaml ]; then
    kubectl delete -f build/manifests/manifest.yaml --ignore-not-found || true
  elif [ -f k8s/manifest.yaml ]; then
    kubectl delete -f k8s/manifest.yaml --ignore-not-found || true
  fi
  kubectl delete namespace kube-agentfs-system --ignore-not-found || true
else
  echo "No active or reachable Kubernetes cluster configured. Skipping kubectl deletions."
fi

# Clean up local temporary manifests
rm -rf build/manifests

# 4. Destroy KOPS Cluster Resources
echo "Tearing down the KOPS GCE resources for ${KOPS_CLUSTER_NAME}..."
if command -v kops &>/dev/null; then
  kops delete cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" --yes || true
else
  echo "kops CLI not found. Skipping KOPS cluster destruction."
fi

# 5. Delete GCS State Bucket and GAR Repository
echo "Deleting KOPS GCS state bucket ${KOPS_STATE_STORE}..."
gcloud storage buckets delete "${KOPS_STATE_STORE}" --quiet || true

echo "Deleting Artifact Registry Repository ${GAR_REPOSITORY}..."
gcloud artifacts repositories delete "${GAR_REPOSITORY}" \
  --location="${GAR_LOCATION}" \
  --project="${GCP_PROJECT}" \
  --quiet || true

echo "=== Teardown finished! ==="
