#!/bin/bash
# Description: Automated GKE runbook deployment for in-cluster-storage
# Pinned settings: Project: barni-cnrm-20260529, Cluster: ics-1
set -euo pipefail

# Configuration variables (defaulted or overridden)
PROJECT="${GOOGLE_CLOUD_PROJECT:-barni-cnrm-20260529}"
REGION="${CLOUDSDK_COMPUTE_REGION:-us-central1}"
ZONE="${CLOUDSDK_COMPUTE_ZONE:-us-central1-a}"
CLUSTER="${GKE_CLUSTER:-ics-1}"
GAR_LOCATION="${GAR_LOCATION:-us-central1}"
GAR_REPOSITORY="${GAR_REPOSITORY:-in-cluster-storage}"
IMAGE_TAG="${IMAGE_TAG:-latest}"

REGISTRY="${GAR_LOCATION}-docker.pkg.dev/${PROJECT}/${GAR_REPOSITORY}"

echo "=== Deploying to GCP Project: ${PROJECT} ==="
echo "=== Region: ${REGION}, Zone: ${ZONE} ==="
echo "=== Cluster: ${CLUSTER} ==="
echo "=== GAR Repository: ${GAR_REPOSITORY} at ${GAR_LOCATION} ==="

# 1. Enable APIs
echo "Enabling necessary GCP APIs..."
gcloud services enable container.googleapis.com artifactregistry.googleapis.com --project="${PROJECT}"

# 2. Check and Create GKE Cluster if not exists
echo "Checking if GKE cluster ${CLUSTER} exists..."
if ! gcloud container clusters describe "${CLUSTER}" --zone="${ZONE}" --project="${PROJECT}" >/dev/null 2>&1; then
  echo "Cluster ${CLUSTER} does not exist. Creating standard cluster..."
  gcloud container clusters create "${CLUSTER}" \
    --project="${PROJECT}" \
    --zone="${ZONE}" \
    --num-nodes=3 \
    --machine-type=e2-standard-2 \
    --quiet
else
  echo "Cluster ${CLUSTER} already exists."
fi

# 3. Configure credentials
echo "Configuring kubectl credentials for ${CLUSTER}..."
gcloud container clusters get-credentials "${CLUSTER}" \
  --zone="${ZONE}" \
  --project="${PROJECT}"

# 4. Create GAR Repository if not exists
echo "Checking if GAR repository ${GAR_REPOSITORY} exists..."
if ! gcloud artifacts repositories describe "${GAR_REPOSITORY}" --location="${GAR_LOCATION}" --project="${PROJECT}" >/dev/null 2>&1; then
  echo "GAR repository ${GAR_REPOSITORY} does not exist. Creating..."
  gcloud artifacts repositories create "${GAR_REPOSITORY}" \
    --repository-format=docker \
    --location="${GAR_LOCATION}" \
    --description="In-cluster storage subsystem images" \
    --project="${PROJECT}"
else
  echo "GAR repository ${GAR_REPOSITORY} already exists."
fi

# 5. Authenticate Docker with Artifact Registry
echo "Authenticating Docker for ${GAR_LOCATION}-docker.pkg.dev..."
gcloud auth configure-docker "${GAR_LOCATION}-docker.pkg.dev" --quiet --project="${PROJECT}"

# 6. Build and Push Container Images
echo "Building and pushing subsystem images using Google Cloud Build..."
gcloud builds submit --config=cloudbuild.yaml \
  --substitutions=_REGISTRY="${REGISTRY}",_IMAGE_TAG="${IMAGE_TAG}" \
  --project="${PROJECT}" \
  --timeout=900s \
  .

# 7. Update Manifests and Deploy Subsystems
echo "Updating manifests..."
mkdir -p build/manifests

sed -e "s|image: agentfs-controller:latest|image: ${REGISTRY}/agentfs-controller:${IMAGE_TAG}|g" \
    -e "s|image: agentfs-node-daemon:latest|image: ${REGISTRY}/agentfs-node-daemon:${IMAGE_TAG}|g" \
    k8s/manifest.yaml > build/manifests/manifest.yaml

sed -e "s|image: objectfs-controller:latest|image: ${REGISTRY}/objectfs-controller:${IMAGE_TAG}|g" \
    -e "s|image: objectfs-node-daemon:latest|image: ${REGISTRY}/objectfs-node-daemon:${IMAGE_TAG}|g" \
    k8s/objectfs.yaml > build/manifests/objectfs.yaml

sed -e "s|image: wal-buffer:latest|image: ${REGISTRY}/wal-buffer:${IMAGE_TAG}|g" \
    k8s/wal.yaml > build/manifests/wal.yaml

echo "Deploying namespaces and manifests..."
kubectl create namespace kube-agentfs-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace kube-objectfs-system --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f build/manifests/manifest.yaml
kubectl apply -f build/manifests/wal.yaml
kubectl apply -f build/manifests/objectfs.yaml

# 8. Deploy CAS CSI Driver
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
          args: ["--v=5", "--csi-address=\$(ADDRESS)", "--kubelet-registration-path=\$(DRIVER_REG_SOCK_PATH)"]
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
          args: ["--v=5", "--endpoint=unix:///csi/csi.sock", "--nodeid=\$(NODE_ID)", "--storage-path=/var/cache/cas", "--controller-address=agentfs-controller.kube-agentfs-system.svc.cluster.local:50051"]
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

echo "=== Deployment finished! ==="
