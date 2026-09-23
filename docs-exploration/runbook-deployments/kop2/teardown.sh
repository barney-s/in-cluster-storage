#!/bin/bash
# Description: Automated GKE Standard runbook teardown for in-cluster-storage (instance: kop2)
# Pinned settings: Project: barni-cnrm-20260529, Cluster: ics-kop2, Prefix: ics-kop2
set -euo pipefail

# Get the directory of this script to source params.env correctly
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/params.env"

echo "=== Tearing down GKE Standard Project: ${GCP_PROJECT} ==="
echo "=== Region: ${GCP_REGION}, Zone: ${GCP_ZONE} ==="
echo "=== Cluster Name: ${CLUSTER_NAME} ==="
echo "=== GAR Repository: ${GAR_REPOSITORY} at ${GAR_LOCATION} ==="
echo "=== Resource Prefix: ${RESOURCE_PREFIX} ==="

# 1. Connect kubectl to the GKE cluster to ensure we can delete resources if cluster is online
echo "Attempting to get GKE credentials for clean in-cluster teardown..."
if gcloud container clusters get-credentials "${CLUSTER_NAME}" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" >/dev/null 2>&1; then
  echo "Connected to cluster. Deleting Kubernetes storage subsystems..."

  # Delete CAS CSI Driver
  echo "Deleting CAS CSI driver..."
  kubectl delete csidriver cas.labs.gke.io --ignore-not-found || true
  kubectl delete daemonset cas-node-daemon -n kube-agentfs-system --ignore-not-found || true

  # Delete ObjectFS and WAL Buffer
  echo "Deleting ObjectFS and WAL workloads..."
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

  # Delete AgentFS
  echo "Deleting AgentFS workloads..."
  if [ -f build/manifests/manifest.yaml ]; then
    kubectl delete -f build/manifests/manifest.yaml --ignore-not-found || true
  elif [ -f k8s/manifest.yaml ]; then
    kubectl delete -f k8s/manifest.yaml --ignore-not-found || true
  fi

  kubectl delete namespace kube-agentfs-system --ignore-not-found || true
else
  echo "Could not connect to GKE cluster or cluster already deleted. Skipping in-cluster workload deletions."
fi

# Clean up local temporary manifests
rm -rf build/manifests

# 2. Delete GKE Cluster
echo "Deleting GKE Cluster ${CLUSTER_NAME}..."
if gcloud container clusters describe "${CLUSTER_NAME}" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" >/dev/null 2>&1; then
  gcloud container clusters delete "${CLUSTER_NAME}" \
    --zone="${GCP_ZONE}" \
    --project="${GCP_PROJECT}" \
    --quiet
else
  echo "GKE Cluster ${CLUSTER_NAME} does not exist or is already deleted."
fi

# 3. Delete Artifact Registry Repository
echo "Deleting Artifact Registry Repository ${GAR_REPOSITORY}..."
if gcloud artifacts repositories describe "${GAR_REPOSITORY}" --location="${GAR_LOCATION}" --project="${GCP_PROJECT}" >/dev/null 2>&1; then
  gcloud artifacts repositories delete "${GAR_REPOSITORY}" \
    --location="${GAR_LOCATION}" \
    --project="${GCP_PROJECT}" \
    --quiet
else
  echo "Artifact Registry Repository ${GAR_REPOSITORY} does not exist or is already deleted."
fi

echo "=== Teardown finished! ==="
