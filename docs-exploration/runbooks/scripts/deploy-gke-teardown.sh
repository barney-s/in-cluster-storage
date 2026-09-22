#!/bin/bash
# Description: Automated GKE runbook teardown for in-cluster-storage
# Pinned settings: Project: barni-cnrm-20260529, Cluster: ics-1
set -euo pipefail

# Configuration variables (defaulted or overridden)
PROJECT="${GOOGLE_CLOUD_PROJECT:-barni-cnrm-20260529}"
REGION="${CLOUDSDK_COMPUTE_REGION:-us-central1}"
ZONE="${CLOUDSDK_COMPUTE_ZONE:-us-central1-a}"
CLUSTER="${GKE_CLUSTER:-ics-1}"
GAR_LOCATION="${GAR_LOCATION:-us-central1}"
GAR_REPOSITORY="${GAR_REPOSITORY:-in-cluster-storage}"

echo "=== Tearing down GCP Project: ${PROJECT} ==="
echo "=== Region: ${REGION}, Zone: ${ZONE} ==="
echo "=== Cluster: ${CLUSTER} ==="
echo "=== GAR Repository: ${GAR_REPOSITORY} at ${GAR_LOCATION} ==="

# Configure credentials
gcloud container clusters get-credentials "${CLUSTER}" \
  --zone="${ZONE}" \
  --project="${PROJECT}" || true

# 0. Clean up in-cluster Job and ConfigMap
echo "Deleting in-cluster Job and ConfigMap..."
kubectl delete job apply-manifests-job --namespace=default --ignore-not-found || true
kubectl delete configmap manifests-config --namespace=default --ignore-not-found || true

# 1. Delete CAS
echo "Deleting CAS..."
kubectl delete csidriver cas.labs.gke.io --ignore-not-found || true
kubectl delete daemonset cas-node-daemon -n kube-agentfs-system --ignore-not-found || true

# 2. Delete ObjectFS and WAL Buffer
echo "Deleting ObjectFS and WAL..."
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

# 3. Delete AgentFS
echo "Deleting AgentFS..."
if [ -f build/manifests/manifest.yaml ]; then
  kubectl delete -f build/manifests/manifest.yaml --ignore-not-found || true
else
  kubectl delete -f k8s/manifest.yaml --ignore-not-found || true
fi
kubectl delete namespace kube-agentfs-system --ignore-not-found || true

# Clean up local temporary manifests
rm -rf build/manifests

# 4. Delete GKE Cluster
echo "Deleting GKE Cluster ${CLUSTER}..."
gcloud container clusters delete "${CLUSTER}" \
  --zone="${ZONE}" \
  --project="${PROJECT}" \
  --quiet || true

# 5. Delete GAR Repository
echo "Deleting Artifact Registry Repository ${GAR_REPOSITORY}..."
gcloud artifacts repositories delete "${GAR_REPOSITORY}" \
  --location="${GAR_LOCATION}" \
  --project="${PROJECT}" \
  --quiet || true

echo "=== Teardown finished! ==="
