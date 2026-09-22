#!/bin/bash
# Description: KOPS on GCE runbook deployment for in-cluster-storage (ics-2)
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

echo "=== Deploying to GCP Project: ${PROJECT} ==="
echo "=== Region: ${REGION}, Zone: ${ZONE} ==="
echo "=== KOPS Cluster Name: ${KOPS_CLUSTER_NAME} ==="
echo "=== KOPS State Store: ${KOPS_STATE_STORE} ==="
echo "=== GAR Repository: ${GAR_REPOSITORY} at ${GAR_LOCATION} ==="
echo "=== Image Registry Prefix: ${REGISTRY} ==="

# Export variables for child processes/tools
export GCP_PROJECT="${PROJECT}"
export GCP_REGION="${REGION}"
export GCP_ZONE="${ZONE}"
export KOPS_CLUSTER_NAME="${KOPS_CLUSTER_NAME}"
export KOPS_STATE_STORE="${KOPS_STATE_STORE}"
export GAR_LOCATION="${GAR_LOCATION}"
export GAR_REPOSITORY="${GAR_REPOSITORY}"
export IMAGE_TAG="${IMAGE_TAG}"
export REGISTRY="${REGISTRY}"

# 1. Authenticate and set target project
echo "Configuring gcloud project to ${PROJECT}..."
gcloud config set project "${PROJECT}"

# 2. Check and Create KOPS State Store Bucket
echo "Checking if KOPS state store GCS bucket ${KOPS_STATE_STORE} exists..."
if ! gsutil ls -b "${KOPS_STATE_STORE}" >/dev/null 2>&1; then
  echo "GCS bucket ${KOPS_STATE_STORE} does not exist. Creating..."
  gsutil mb -p "${PROJECT}" -l "${REGION}" "${KOPS_STATE_STORE}"
else
  echo "GCS bucket ${KOPS_STATE_STORE} already exists."
fi

# 3. Check and Create KOPS Cluster
echo "Checking if KOPS cluster ${KOPS_CLUSTER_NAME} exists..."
if ! kops get cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" >/dev/null 2>&1; then
  echo "KOPS cluster ${KOPS_CLUSTER_NAME} does not exist. Creating and provisioning..."
  kops create cluster \
    --name="${KOPS_CLUSTER_NAME}" \
    --zones="${ZONE}" \
    --state="${KOPS_STATE_STORE}" \
    --project="${PROJECT}" \
    --cloud=gce \
    --node-count=3 \
    --node-size=e2-standard-2 \
    --master-size=e2-standard-2 \
    --yes
else
  echo "KOPS cluster ${KOPS_CLUSTER_NAME} already exists."
fi

echo "Validating cluster rollout (waiting up to 10m)..."
kops validate cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" --wait 10m

# 4. Configure KOPS Node IAM Permissions for GAR access
echo "Configuring node IAM permissions for Google Artifact Registry access..."
COMPUTE_SVC_ACCT=$(gcloud iam service-accounts list --project="${PROJECT}" --filter="displayName:Compute Engine default service account" --format="value(email)" | head -n1)
if [ -n "${COMPUTE_SVC_ACCT}" ]; then
  echo "Granting roles/artifactregistry.reader role to service account: ${COMPUTE_SVC_ACCT}"
  gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${COMPUTE_SVC_ACCT}" \
    --role="roles/artifactregistry.reader"
else
  echo "Warning: Could not identify Compute Engine default service account. Skipping IAM role assignment." >&2
fi

# 5. Create GAR Repository if not exists
echo "Checking if GAR repository ${GAR_REPOSITORY} exists in ${GAR_LOCATION}..."
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

# 6. Authenticate Docker with Artifact Registry
echo "Authenticating Docker for ${GAR_LOCATION}-docker.pkg.dev..."
gcloud auth configure-docker "${GAR_LOCATION}-docker.pkg.dev" --quiet --project="${PROJECT}"

# 7. Build and Push Container Images
echo "Building and pushing container images to ${REGISTRY}..."
docker build -t "${REGISTRY}/agentfs-controller:${IMAGE_TAG}" -f images/agentfs-controller/Dockerfile .
docker build -t "${REGISTRY}/agentfs-node-daemon:${IMAGE_TAG}" -f images/agentfs-node-daemon/Dockerfile .
docker build -t "${REGISTRY}/objectfs-controller:${IMAGE_TAG}" -f images/objectfs-controller/Dockerfile .
docker build -t "${REGISTRY}/objectfs-node-daemon:${IMAGE_TAG}" -f images/objectfs-node-daemon/Dockerfile .
docker build -t "${REGISTRY}/wal-buffer:${IMAGE_TAG}" -f images/wal-buffer/Dockerfile .
docker build -t "${REGISTRY}/cas-node-daemon:${IMAGE_TAG}" -f images/cas-node-daemon/Dockerfile .

docker push "${REGISTRY}/agentfs-controller:${IMAGE_TAG}"
docker push "${REGISTRY}/agentfs-node-daemon:${IMAGE_TAG}"
docker push "${REGISTRY}/objectfs-controller:${IMAGE_TAG}"
docker push "${REGISTRY}/objectfs-node-daemon:${IMAGE_TAG}"
docker push "${REGISTRY}/wal-buffer:${IMAGE_TAG}"
docker push "${REGISTRY}/cas-node-daemon:${IMAGE_TAG}"

# 8. Update Manifests and Deploy Subsystems
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

# 9. Deploy CAS CSI Driver (Optional from runbook)
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
