#!/bin/bash
# Description: Automated KOPS on GCE runbook deployment for in-cluster-storage
# Change: Enabled Uniform Bucket Level Access/storage.admin on state bucket, and removed escaping of $(VAR) inside literal EOF heredoc to prevent YAML syntax errors.
# Pinned settings: Project: barni-cnrm-20260529, Cluster: ics3
set -euo pipefail

# Get the directory of this script to source params.env correctly
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/params.env"

echo "=== Deploying KOPS on GCE Project: ${GCP_PROJECT} ==="
echo "=== Region: ${GCP_REGION}, Zone: ${GCP_ZONE} ==="
echo "=== Cluster: ${CLUSTER} ==="
echo "=== KOPS Cluster Name: ${KOPS_CLUSTER_NAME} ==="
echo "=== KOPS State Store: ${KOPS_STATE_STORE} ==="
echo "=== GAR Repository: ${GAR_REPOSITORY} at ${GAR_LOCATION} ==="

# 1. Configure target GCP project
echo "Setting target GCP project..."
gcloud config set project "${GCP_PROJECT}"

# 2. Check and Create GCS State Store Bucket
echo "Checking if KOPS state GCS bucket ${KOPS_STATE_STORE} exists..."
if ! gcloud storage buckets describe "${KOPS_STATE_STORE}" >/dev/null 2>&1; then
  echo "GCS state store bucket does not exist. Creating..."
  gcloud storage buckets create "${KOPS_STATE_STORE}" --project="${GCP_PROJECT}" --location="${GCP_REGION}" --uniform-bucket-level-access
else
  echo "GCS state store bucket already exists. Ensuring Uniform Bucket Level Access is enabled..."
  gcloud storage buckets update "${KOPS_STATE_STORE}" --uniform-bucket-level-access
fi

# Dynamically grant storage.admin to the active workload identity principal to avoid HTTP 403 / 412 errors during KOPS reads
echo "Ensuring active identity has storage.admin role on the state store bucket..."
ACTIVE_PRINCIPAL=$(gcloud projects get-iam-policy "${GCP_PROJECT}" --format="value(bindings.members)" | tr -d "['\",]" | grep -o 'principal://[^ ]*' | sort -u | head -n 1 || true)
if [ -n "${ACTIVE_PRINCIPAL}" ]; then
  echo "Granting roles/storage.admin to ${ACTIVE_PRINCIPAL}..."
  gcloud storage buckets add-iam-policy-binding "${KOPS_STATE_STORE}" \
    --member="${ACTIVE_PRINCIPAL}" \
    --role="roles/storage.admin" \
    --quiet || true
else
  echo "Could not dynamically determine active workload identity principal. Skipping explicit grant."
fi

# 3. Authenticate Docker with Artifact Registry
echo "Authenticating Docker for ${GAR_LOCATION}-docker.pkg.dev..."
gcloud auth configure-docker "${GAR_LOCATION}-docker.pkg.dev" --quiet

# 4. Create and Provision the KOPS Cluster
echo "Checking if KOPS cluster ${KOPS_CLUSTER_NAME} exists..."
if ! kops get cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" >/dev/null 2>&1; then
  echo "KOPS cluster does not exist. Generating configuration and provisioning..."
  kops create cluster \
    --name="${KOPS_CLUSTER_NAME}" \
    --zones="${GCP_ZONE}" \
    --state="${KOPS_STATE_STORE}" \
    --project="${GCP_PROJECT}" \
    --cloud=gce \
    --node-count=3 \
    --node-size=e2-standard-2 \
    --master-size=e2-standard-2 \
    --yes
else
  echo "KOPS cluster already exists. Ensuring configuration is applied and kubeconfig is updated..."
  kops update cluster \
    --name="${KOPS_CLUSTER_NAME}" \
    --state="${KOPS_STATE_STORE}" \
    --yes
fi

# Validate cluster rollout (takes 5-10 minutes)
echo "Validating KOPS cluster rollout..."
kops validate cluster --state="${KOPS_STATE_STORE}" --wait 10m

# 5. Configure KOPS Node IAM Permissions for GAR access
echo "Configuring KOPS compute instance IAM permissions for GAR access..."
# 5a. Default Compute Service Account
COMPUTE_SVC_ACCT=$(gcloud iam service-accounts list --filter="displayName:Compute Engine default service account" --format="value(email)" --project="${GCP_PROJECT}")
if [ -n "${COMPUTE_SVC_ACCT}" ]; then
  echo "Granting roles/artifactregistry.reader to default Compute service account ${COMPUTE_SVC_ACCT}..."
  gcloud projects add-iam-policy-binding "${GCP_PROJECT}" \
    --member="serviceAccount:${COMPUTE_SVC_ACCT}" \
    --role="roles/artifactregistry.reader" \
    --quiet || true
fi

# 5b. Dedicated KOPS Node and Control-Plane Service Accounts
KOPS_NODE_SA="node-${KOPS_CLUSTER_NAME//./-}@${GCP_PROJECT}.iam.gserviceaccount.com"
echo "Granting roles/artifactregistry.reader to KOPS node service account ${KOPS_NODE_SA}..."
gcloud projects add-iam-policy-binding "${GCP_PROJECT}" \
  --member="serviceAccount:${KOPS_NODE_SA}" \
  --role="roles/artifactregistry.reader" \
  --quiet || true

KOPS_CP_SA="control-plane-${KOPS_CLUSTER_NAME//./-}@${GCP_PROJECT}.iam.gserviceaccount.com"
echo "Granting roles/artifactregistry.reader to KOPS control-plane service account ${KOPS_CP_SA}..."
gcloud projects add-iam-policy-binding "${GCP_PROJECT}" \
  --member="serviceAccount:${KOPS_CP_SA}" \
  --role="roles/artifactregistry.reader" \
  --quiet || true

# 6. Create Artifact Registry Repository if not exists
echo "Checking if Artifact Registry repository ${GAR_REPOSITORY} exists..."
if ! gcloud artifacts repositories describe "${GAR_REPOSITORY}" --location="${GAR_LOCATION}" --project="${GCP_PROJECT}" >/dev/null 2>&1; then
  echo "GAR repository does not exist. Creating..."
  gcloud artifacts repositories create "${GAR_REPOSITORY}" \
    --repository-format=docker \
    --location="${GAR_LOCATION}" \
    --description="In-cluster storage subsystem images" \
    --project="${GCP_PROJECT}"
else
  echo "GAR repository already exists."
fi

# 7. Build and Push Container Images using Parallel Google Cloud Build (Path A)
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

# 8. Update Manifests and Deploy Subsystems
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

# 9. Deploy CAS CSI Driver
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

# 10. Verification Checks
echo "=== Running Verification Checks ==="
echo "CSI Drivers:"
kubectl get csidrivers | grep -E 'agentfs|objectfs|cas' || true

echo "AgentFS & CAS Pods in kube-agentfs-system:"
kubectl get pods -n kube-agentfs-system -o wide || true

echo "ObjectFS Pods in kube-objectfs-system:"
kubectl get pods -n kube-objectfs-system -o wide || true

echo "=== Deployment finished successfully! ==="
