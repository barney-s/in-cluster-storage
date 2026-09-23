#!/bin/bash
# Description: Automated GKE runbook teardown for in-cluster-storage
# Pinned settings: Project: barni-cnrm-20260529, Cluster: ics-1
set -euo pipefail

# Determine script directory and repository root
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# Source params.env if it exists
if [ -f "${SCRIPT_DIR}/params.env" ]; then
  source "${SCRIPT_DIR}/params.env"
else
  # Configuration variables (defaulted or overridden)
  PROJECT="${GOOGLE_CLOUD_PROJECT:-barni-cnrm-20260529}"
  REGION="${CLOUDSDK_COMPUTE_REGION:-us-central1}"
  ZONE="${CLOUDSDK_COMPUTE_ZONE:-us-central1-a}"
  CLUSTER="${GKE_CLUSTER:-ics-1}"
  GAR_LOCATION="${GAR_LOCATION:-us-central1}"
  GAR_REPOSITORY="${GAR_REPOSITORY:-in-cluster-storage}"
fi

# Change directory to the repository root
cd "${REPO_ROOT}"

echo "=== Tearing down GCP Project: ${PROJECT} ==="
echo "=== Region: ${REGION}, Zone: ${ZONE} ==="
echo "=== Cluster: ${CLUSTER} ==="
echo "=== GAR Repository: ${GAR_REPOSITORY} at ${GAR_LOCATION} ==="

# Check cluster status before trying to interact with it
echo "Checking GKE Cluster status..."
CLUSTER_STATUS=""
if gcloud container clusters describe "${CLUSTER}" --zone="${ZONE}" --project="${PROJECT}" >/dev/null 2>&1; then
  CLUSTER_STATUS=$(gcloud container clusters describe "${CLUSTER}" --zone="${ZONE}" --project="${PROJECT}" --format="value(status)" 2>/dev/null || echo "")
fi

echo "GKE Cluster status is: '${CLUSTER_STATUS}'"

if [ "${CLUSTER_STATUS}" = "RUNNING" ]; then
  # Configure credentials
  gcloud container clusters get-credentials "${CLUSTER}" \
    --zone="${ZONE}" \
    --project="${PROJECT}" || true

  # 0. Clean up in-cluster Job and ConfigMap
  echo "Deleting in-cluster Job and ConfigMap..."
  kubectl delete job apply-manifests-job --namespace=default --ignore-not-found --request-timeout=15s || true
  kubectl delete configmap manifests-config --namespace=default --ignore-not-found --request-timeout=15s || true

  # 1. Delete CAS
  echo "Deleting CAS..."
  kubectl delete csidriver cas.labs.gke.io --ignore-not-found --request-timeout=15s || true
  kubectl delete daemonset cas-node-daemon -n kube-agentfs-system --ignore-not-found --request-timeout=15s || true

  # 2. Delete ObjectFS and WAL Buffer
  echo "Deleting ObjectFS and WAL..."
  if [ -f build/manifests/objectfs.yaml ]; then
    kubectl delete -f build/manifests/objectfs.yaml --ignore-not-found --request-timeout=15s || true
  else
    kubectl delete -f k8s/objectfs.yaml --ignore-not-found --request-timeout=15s || true
  fi

  if [ -f build/manifests/wal.yaml ]; then
    kubectl delete -f build/manifests/wal.yaml --ignore-not-found --request-timeout=15s || true
  else
    kubectl delete -f k8s/wal.yaml --ignore-not-found --request-timeout=15s || true
  fi

  kubectl delete namespace kube-objectfs-system --ignore-not-found --request-timeout=15s || true

  # 3. Delete AgentFS
  echo "Deleting AgentFS..."
  if [ -f build/manifests/manifest.yaml ]; then
    kubectl delete -f build/manifests/manifest.yaml --ignore-not-found --request-timeout=15s || true
  else
    kubectl delete -f k8s/manifest.yaml --ignore-not-found --request-timeout=15s || true
  fi
  kubectl delete namespace kube-agentfs-system --ignore-not-found --request-timeout=15s || true
else
  echo "Skipping Kubernetes-level resource deletion because the cluster is not in RUNNING status."
fi

# Clean up local temporary manifests
rm -rf build/manifests

# 4. Delete GKE Cluster
if [ -n "${CLUSTER_STATUS}" ] && [ "${CLUSTER_STATUS}" != "STOPPING" ]; then
  echo "Deleting GKE Cluster ${CLUSTER}..."
  gcloud container clusters delete "${CLUSTER}" \
    --zone="${ZONE}" \
    --project="${PROJECT}" \
    --quiet || true
else
  echo "GKE Cluster ${CLUSTER} is not active or is already deleting (status: '${CLUSTER_STATUS}'). Skipping deletion."
fi

# 5. Delete GAR Repository
echo "Deleting Artifact Registry Repository ${GAR_REPOSITORY}..."
if gcloud artifacts repositories describe "${GAR_REPOSITORY}" --location="${GAR_LOCATION}" --project="${PROJECT}" >/dev/null 2>&1; then
  gcloud artifacts repositories delete "${GAR_REPOSITORY}" \
    --location="${GAR_LOCATION}" \
    --project="${PROJECT}" \
    --quiet || true
else
  echo "GAR Repository ${GAR_REPOSITORY} does not exist. Skipping."
fi

echo "=== Teardown finished! ==="
