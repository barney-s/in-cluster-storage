#!/bin/bash
# Description: GKE runbook teardown for in-cluster-storage (ics-2)
# Pinned settings: Project: barni-cnrm-20260529, Cluster: ics-1
# Revision: Pivoted target to GKE (ics-1) to match deploy.sh pivot.
set -euo pipefail

# Locate and source parameters
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "${SCRIPT_DIR}/params.env" ]; then
  source "${SCRIPT_DIR}/params.env"
else
  echo "Error: params.env not found at ${SCRIPT_DIR}/params.env" >&2
  exit 1
fi

echo "=== Tearing down GKE Deployment from GCP Project: ${PROJECT} ==="

# 1. Authenticate and get GKE credentials
echo "Configuring gcloud project to ${PROJECT}..."
gcloud config set project "${PROJECT}"

echo "Getting GKE credentials for cluster ics-1..."
gcloud container clusters get-credentials ics-1 --zone=us-central1-a --project="${PROJECT}"

# 2. Delete GKE resources
echo "Deleting Kubernetes resources..."
kubectl delete csidriver cas.labs.gke.io --ignore-not-found
kubectl delete daemonset cas-node-daemon -n kube-agentfs-system --ignore-not-found

if [ -f "build/manifests/objectfs.yaml" ]; then
  kubectl delete -f build/manifests/objectfs.yaml --ignore-not-found || true
fi
if [ -f "build/manifests/wal.yaml" ]; then
  kubectl delete -f build/manifests/wal.yaml --ignore-not-found || true
fi
if [ -f "build/manifests/manifest.yaml" ]; then
  kubectl delete -f build/manifests/manifest.yaml --ignore-not-found || true
fi

kubectl delete namespace kube-objectfs-system --ignore-not-found
kubectl delete namespace kube-agentfs-system --ignore-not-found

echo "Cleaning up local temporary manifests..."
rm -rf build/manifests

echo "=== Teardown finished! ==="
