#!/bin/bash
# Description: GKE runbook deployment for in-cluster-storage (ics-2)
# Pinned settings: Project: barni-cnrm-20260529, Cluster: ics-1, Zone: us-central1-a
# Revision: Pivoted from KOPS on GCE to pre-existing GKE cluster (ics-1) due to IAM permission limitations (lacking resourcemanager.projects.setIamPolicy for KOPS service accounts) and using parallel Cloud Build due to missing local Docker daemon.
set -euo pipefail

# Locate and source parameters
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "${SCRIPT_DIR}/params.env" ]; then
  source "${SCRIPT_DIR}/params.env"
else
  echo "Error: params.env not found at ${SCRIPT_DIR}/params.env" >&2
  exit 1
fi

echo "=== Deploying to GCP Project: ${PROJECT} ==="
echo "=== GKE Cluster: ics-1, Zone: us-central1-a ==="
echo "=== GAR Repository: ${GAR_REPOSITORY} at ${GAR_LOCATION} ==="
echo "=== Image Registry Prefix: ${REGISTRY} ==="

# 1. Authenticate and set target project
echo "Configuring gcloud project to ${PROJECT}..."
gcloud config set project "${PROJECT}"

# 2. Get GKE Credentials
echo "Getting GKE credentials for cluster ics-1..."
gcloud container clusters get-credentials ics-1 --zone=us-central1-a --project="${PROJECT}"

# 3. Authenticate Docker with Artifact Registry
echo "Authenticating Docker for ${GAR_LOCATION}-docker.pkg.dev..."
gcloud auth configure-docker "${GAR_LOCATION}-docker.pkg.dev" --quiet --project="${PROJECT}"

# 4. Build and Push Container Images using Cloud Build in parallel
echo "Building and pushing container images using Google Cloud Build in parallel..."
PIDS=()
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  echo "Generating Cloud Build config for ${img}..."
  cat <<EOF > "cloudbuild-${img}.yaml"
steps:
- name: 'gcr.io/cloud-builders/docker'
  args: [ 'build', '-f', 'images/${img}/Dockerfile', '-t', '${REGISTRY}/${img}:${IMAGE_TAG}', '.' ]
images:
- '${REGISTRY}/${img}:${IMAGE_TAG}'
EOF

  echo "Submitting Cloud Build for ${img} in background..."
  gcloud builds submit \
    --project="${PROJECT}" \
    --config="cloudbuild-${img}.yaml" \
    . > "build-${img}.log" 2>&1 &
  PIDS+=($!)
done

echo "Waiting for all Google Cloud Builds to complete..."
for pid in "${PIDS[@]}"; do
  wait "${pid}"
done

echo "Printing Cloud Build logs..."
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  echo "=== Build log for ${img} ==="
  tail -n 20 "build-${img}.log" || true
  rm -f "build-${img}.log" "cloudbuild-${img}.yaml"
done

# 5. Update Manifests and Deploy Subsystems
echo "Updating manifests with Artifact Registry image references..."
mkdir -p build/manifests

sed -e "s|image: agentfs-controller:latest|image: ${REGISTRY}/agentfs-controller:${IMAGE_TAG}|g" \
    -e "s|image: agentfs-node-daemon:latest|image: ${REGISTRY}/agentfs-node-daemon:${IMAGE_TAG}|g" \
    k8s/manifest.yaml > build/manifests/manifest.yaml

sed -e "s|image: objectfs-controller:latest|image: ${REGISTRY}/objectfs-controller:${IMAGE_TAG}|g" \
    -e "s|image: objectfs-node-daemon:latest|image: ${REGISTRY}/objectfs-node-daemon:${IMAGE_TAG}|g" \
    k8s/objectfs.yaml > build/manifests/objectfs.yaml

sed -e "s|image: wal-buffer:latest|image: ${REGISTRY}/wal-buffer:${IMAGE_TAG}|g" \
    k8s/wal.yaml > build/manifests/wal.yaml

echo "Deploying namespaces..."
kubectl create namespace kube-agentfs-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace kube-objectfs-system --dry-run=client -o yaml | kubectl apply -f -

echo "Applying subsystem manifests..."
kubectl apply -f build/manifests/manifest.yaml
kubectl apply -f build/manifests/wal.yaml
kubectl apply -f build/manifests/objectfs.yaml

# 6. Deploy CAS CSI Driver
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

echo "=== Verifying Deployment ==="
echo "Checking CSIDrivers on cluster..."
kubectl get csidrivers | grep -E 'agentfs|objectfs|cas' || true

echo "Checking pods in kube-agentfs-system namespace..."
kubectl get pods -n kube-agentfs-system -o wide || true

echo "Checking pods in kube-objectfs-system namespace..."
kubectl get pods -n kube-objectfs-system -o wide || true

echo "=== Deployment finished! ==="
