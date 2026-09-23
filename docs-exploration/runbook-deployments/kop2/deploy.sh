#!/bin/bash
# Description: Automated GKE Standard runbook deployment for in-cluster-storage (instance: kop2)
# Pinned settings: Project: barni-cnrm-20260529, Cluster: ics-kop2, Prefix: ics-kop2
set -euo pipefail

# Get the directory of this script to source params.env correctly
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/params.env"

echo "=== Deploying GKE Standard Project: ${GCP_PROJECT} ==="
echo "=== Region: ${GCP_REGION}, Zone: ${GCP_ZONE} ==="
echo "=== Cluster Name: ${CLUSTER_NAME} ==="
echo "=== GAR Repository: ${GAR_REPOSITORY} at ${GAR_LOCATION} ==="
echo "=== Resource Prefix: ${RESOURCE_PREFIX} ==="

# 1. Set Project & Configure Docker Authentication
echo "Setting target GCP project..."
gcloud config set project "${GCP_PROJECT}"

echo "Authenticating Docker for ${GAR_LOCATION}-docker.pkg.dev..."
gcloud auth configure-docker "${GAR_LOCATION}-docker.pkg.dev" --quiet

# 2. Provision GKE Standard Cluster
echo "Checking if GKE cluster ${CLUSTER_NAME} exists..."
if ! gcloud container clusters describe "${CLUSTER_NAME}" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" >/dev/null 2>&1; then
  echo "GKE cluster does not exist. Creating Standard GKE cluster..."
  gcloud container clusters create "${CLUSTER_NAME}" \
    --zone="${GCP_ZONE}" \
    --project="${GCP_PROJECT}" \
    --num-nodes=3 \
    --machine-type="e2-standard-4" \
    --enable-ip-alias \
    --labels=repo-agent-instance="${RESOURCE_PREFIX}" \
    --quiet
else
  echo "GKE cluster ${CLUSTER_NAME} already exists."
fi

# Connect kubectl to the GKE cluster
echo "Connecting kubectl to GKE cluster ${CLUSTER_NAME}..."
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --zone="${GCP_ZONE}" \
  --project="${GCP_PROJECT}"

# 3. Create Artifact Registry Repository
echo "Checking if Artifact Registry repository ${GAR_REPOSITORY} exists..."
if ! gcloud artifacts repositories describe "${GAR_REPOSITORY}" --location="${GAR_LOCATION}" --project="${GCP_PROJECT}" >/dev/null 2>&1; then
  echo "GAR repository does not exist. Creating..."
  gcloud artifacts repositories create "${GAR_REPOSITORY}" \
    --repository-format=docker \
    --location="${GAR_LOCATION}" \
    --description="In-cluster storage subsystem images" \
    --project="${GCP_PROJECT}" \
    --labels=repo-agent-instance="${RESOURCE_PREFIX}"
else
  echo "GAR repository already exists. Updating labels..."
  gcloud artifacts repositories update "${GAR_REPOSITORY}" \
    --location="${GAR_LOCATION}" \
    --project="${GCP_PROJECT}" \
    --update-labels=repo-agent-instance="${RESOURCE_PREFIX}"
fi

# Ensure the cloudbuild source bucket has Uniform Bucket Level Access enabled if it exists
CLOUDBUILD_BUCKET="gs://${GCP_PROJECT}_cloudbuild"
echo "Ensuring Uniform Bucket Level Access is enabled on Cloud Build bucket ${CLOUDBUILD_BUCKET}..."
if gcloud storage buckets describe "${CLOUDBUILD_BUCKET}" >/dev/null 2>&1; then
  gcloud storage buckets update "${CLOUDBUILD_BUCKET}" --uniform-bucket-level-access
else
  # If it doesn't exist, we can pre-create it with UBLA enabled to prevent errors
  gcloud storage buckets create "${CLOUDBUILD_BUCKET}" \
    --project="${GCP_PROJECT}" \
    --location="us" \
    --uniform-bucket-level-access || true
fi

# 4. Build and Push Subsystem Images via Parallel Cloud Build
echo "Submitting parallel Cloud Builds for all six storage subsystems..."
PIDS=()
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  cat <<EOF > "cloudbuild-${img}.yaml"
steps:
- name: 'gcr.io/cloud-builders/docker'
  args: [ 'build', '-f', 'images/${img}/Dockerfile', '-t', '${REGISTRY}/${img}:${IMAGE_TAG}', '.' ]
images:
- '${REGISTRY}/${img}:${IMAGE_TAG}'
EOF

  gcloud builds submit \
    --project="${GCP_PROJECT}" \
    --config="cloudbuild-${img}.yaml" \
    . > "build-${img}.log" 2>&1 &
  PIDS+=($!)
done

# Wait for parallel builds to finish
echo "Waiting for all Google Cloud Builds to complete..."
for pid in "${PIDS[@]}"; do
  wait "${pid}"
done

# Check build logs and clean up configs
echo "Checking cloud build outputs and cleaning up logs..."
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  echo "=== Build log snapshot for ${img} ==="
  tail -n 10 "build-${img}.log" || true
  rm -f "build-${img}.log" "cloudbuild-${img}.yaml"
done

# 5. Compile Manifests and Deploy Subsystems
echo "Updating Kubernetes manifests with compiled GAR image paths..."
mkdir -p build/manifests

sed -e "s|image: agentfs-controller:latest|image: ${REGISTRY}/agentfs-controller:${IMAGE_TAG}|g" \
    -e "s|image: agentfs-node-daemon:latest|image: ${REGISTRY}/agentfs-node-daemon:${IMAGE_TAG}|g" \
    k8s/manifest.yaml > build/manifests/manifest.yaml

sed -e "s|image: objectfs-controller:latest|image: ${REGISTRY}/objectfs-controller:${IMAGE_TAG}|g" \
    -e "s|image: objectfs-node-daemon:latest|image: ${REGISTRY}/objectfs-node-daemon:${IMAGE_TAG}|g" \
    k8s/objectfs.yaml > build/manifests/objectfs.yaml

sed -e "s|image: wal-buffer:latest|image: ${REGISTRY}/wal-buffer:${IMAGE_TAG}|g" \
    k8s/wal.yaml > build/manifests/wal.yaml

echo "Applying target namespaces and subsystem manifests..."
kubectl create namespace kube-agentfs-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace kube-objectfs-system --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f build/manifests/manifest.yaml
kubectl apply -f build/manifests/wal.yaml
kubectl apply -f build/manifests/objectfs.yaml

# 6. Deploy Content Addressable Storage (CAS) CSI Driver
echo "Deploying CAS CSI Driver..."
sed -e "s|image: cas-node-daemon:latest|image: ${REGISTRY}/cas-node-daemon:${IMAGE_TAG}|g" <<'EOF' | kubectl apply -f -
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: cas.labs.gke.io
spec:
  attachRequired: false
  podInfoOnMount: true
  volumeLifecycleModes: [Ephemeral]
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: cas-node-daemon
  namespace: kube-agentfs-system
spec:
  selector:
    matchLabels:
      app: cas-node-daemon
  template:
    metadata:
      labels:
        app: cas-node-daemon
    spec:
      hostPID: true
      containers:
        - name: node-driver-registrar
          image: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.13.0
          args: ["--v=5", "--csi-address=$(ADDRESS)", "--kubelet-registration-path=$(DRIVER_REG_SOCK_PATH)"]
          env:
            - name: ADDRESS
              value: /csi/csi.sock
            - name: DRIVER_REG_SOCK_PATH
              value: /var/lib/kubelet/plugins/cas.labs.gke.io/csi.sock
          volumeMounts:
            - name: socket-dir
              mountPath: /csi
            - name: registration-dir
              mountPath: /registration
        - name: cas-node-daemon
          securityContext:
            privileged: true
            capabilities:
              add: ["SYS_ADMIN"]
          image: cas-node-daemon:latest
          imagePullPolicy: IfNotPresent
          args: ["--v=5", "--endpoint=unix:///csi/csi.sock", "--nodeid=$(NODE_ID)", "--storage-path=/var/cache/cas", "--controller-address=agentfs-controller.kube-agentfs-system.svc.cluster.local:50051"]
          env:
            - name: NODE_ID
              valueFrom: { fieldRef: { fieldPath: spec.nodeName } }
          volumeMounts:
            - name: socket-dir
              mountPath: /csi
            - name: kubelet-dir
              mountPath: /var/lib/kubelet
              mountPropagation: "Bidirectional"
            - name: storage-dir
              mountPath: /var/cache/cas
              mountPropagation: "Bidirectional"
      volumes:
        - name: socket-dir
          hostPath: { path: /var/lib/kubelet/plugins/cas.labs.gke.io/, type: DirectoryOrCreate }
        - name: registration-dir
          hostPath: { path: /var/lib/kubelet/plugins_registry/, type: Directory }
        - name: kubelet-dir
          hostPath: { path: /var/lib/kubelet, type: Directory }
        - name: storage-dir
          hostPath: { path: /var/cache/cas, type: DirectoryOrCreate }
EOF

# 7. Verification Checks
echo "=== Running Verification Checks ==="
echo "CSI Drivers:"
kubectl get csidrivers | grep -E 'agentfs|objectfs|cas' || true

echo "AgentFS & CAS Pods in kube-agentfs-system:"
kubectl get pods -n kube-agentfs-system -o wide || true

echo "ObjectFS Pods in kube-objectfs-system:"
kubectl get pods -n kube-objectfs-system -o wide || true

echo "=== Deployment finished successfully! ==="
