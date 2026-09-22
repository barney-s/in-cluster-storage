# Runbook: Deploying In-Cluster Storage Subsystems to KOPS on GCE

*Changed since last revision: Updated feasibility checklist to reflect newly installed local kops tool and granted GCP IAM permissions (resourcemanager.projects.setIamPolicy) under active Workload Identity, and confirmed Google Cloud Build is the verified and fully operational build path for this environment.*

> ✓ **REQUIREMENTS MET IN CURRENT WORKSPACE**: All required CLI tools (`kops`, `kubectl`, `gcloud`, `go`) are installed, and GCP IAM permissions (`resourcemanager.projects.setIamPolicy`) are fully granted to the active Workload Identity account. While the local Docker daemon is unreachable, the Google Cloud Build path (Path A) is verified and fully operational.

## What this needs

**Tier**: 2 — CSI driver node-daemons require host mounts (`/var/lib/kubelet`), privileged DaemonSets, hostPID, and kubelet plugin socket registration, which can only run on a full Kubernetes cluster with access to actual worker nodes (such as KOPS on GCE) and cannot land in a vcluster.

*Assumptions*: Standard KOPS-managed Kubernetes clusters on Google Compute Engine (GCE) are used. The control plane and worker nodes run as native GCE VMs. Container images are built locally, via native `ap` CLI, or using Google Cloud Build, then pushed to Google Artifact Registry (GAR). KOPS GCE instances are granted IAM access to pull from GAR.

### Feasibility Checklist (Probed in Workspace)

| Tool / Permission | Status | Type | Purpose / Remediation Command |
| :--- | :---: | :--- | :--- |
| **gcloud CLI** | ✓ | Tool | Authenticates with GCP. Found at `/usr/bin/gcloud`. |
| **Go SDK (v1.27.1+)** | ✓ | Tool | Invokes repository build/lint tools. Found at `/usr/local/go/bin/go`. |
| **kubectl** | ✓ | Tool | Manages Kubernetes cluster resources. Found at `/usr/bin/kubectl`. |
| **kops CLI (v1.28+)** | ✓ | Tool | Manages KOPS cluster lifecycle. Verified at `/usr/local/bin/kops` (v1.28.2). |
| **Docker Daemon** | **✗ UNREACHABLE** | Tool | Local container compilation. CLI present at `/usr/bin/docker`, but daemon is unreachable in this environment.<br>*Alternative*: Build via parallel Google Cloud Build jobs using `gcloud builds submit` (fully operational). |
| **GCP Project** | ✓ | IAM | Access target project: `barni-cnrm-20260529`. |
| **iam.serviceAccounts.create** | ✓ | IAM | Create KOPS control plane/worker node service accounts. |
| **roles/artifactregistry.admin** | ✓ | IAM | Create Artifact Registry repositories and push/pull container images. |
| **roles/storage.admin** | ✓ | IAM | Create and manage the KOPS GCS state store bucket. |
| **resourcemanager.projects.setIamPolicy** | ✓ | IAM | Bind GCP IAM roles to KOPS service accounts. Fully granted via `roles/owner` binding on active Workload Identity (`cnrm-barni-1.svc.id.goog`). |

- **Teardown Cost**: GCE VM usage charges (3x `e2-standard-2` workers and 1x `e2-standard-2` master), GCS bucket storage, and Artifact Registry rates apply until cluster and registries are deleted.

## Preconditions

Set the following environment variables. Sourced by default to match the active workspace settings:

```bash
# Target GCP project ID
export GCP_PROJECT="barni-cnrm-20260529"

# GCP/GCE Regional Configuration
export GCP_REGION="us-central1"
export GCP_ZONE="us-central1-a"

# KOPS Cluster Configuration
export CLUSTER="ics2"
export KOPS_CLUSTER_NAME="${CLUSTER}.k8s.local" # Gossip-based cluster name
export KOPS_STATE_STORE="gs://${GCP_PROJECT}-${CLUSTER}-kops-state" # GCS bucket for KOPS state

# Subsystems Deployment Configuration
export GAR_LOCATION="us-central1"
export GAR_REPOSITORY="in-cluster-storage"
export IMAGE_TAG="latest"

# Derived full image registry prefix
export REGISTRY="${GAR_LOCATION}-docker.pkg.dev/${GCP_PROJECT}/${GAR_REPOSITORY}"
```

1. Authenticate to Google Cloud and set the target project:
   ```bash
   gcloud auth login
   gcloud config set project "${GCP_PROJECT}"
   ```

2. Create the GCS bucket for KOPS State Store (if not already created):
   ```bash
   gcloud storage buckets create "${KOPS_STATE_STORE}" --project="${GCP_PROJECT}" --location="${GCP_REGION}"
   ```

3. Authenticate Docker with Google Artifact Registry:
   ```bash
   gcloud auth configure-docker "${GAR_LOCATION}-docker.pkg.dev" --quiet
   ```

## Steps

### Path — KOPS on GCE (tier 2)

#### 1. Create and Provision the KOPS Cluster
Initialize and apply the cluster configuration in GCE using KOPS.

```bash
# Generate the KOPS cluster config in GCP
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

# Validate cluster rollout (takes 5-10 minutes to initialize instances and services)
kops validate cluster --state="${KOPS_STATE_STORE}" --wait 10m
```

#### 2. Configure KOPS Node IAM Permissions for GAR access
Bind the Artifact Registry Reader role to the Compute default service account so KOPS worker nodes can pull private images:

```bash
# Retrieve the GCE default service account email
COMPUTE_SVC_ACCT=$(gcloud iam service-accounts list --filter="displayName:Compute Engine default service account" --format="value(email)")

# Bind the Artifact Registry Reader role to the Compute default service account
gcloud projects add-iam-policy-binding "${GCP_PROJECT}" \
  --member="serviceAccount:${COMPUTE_SVC_ACCT}" \
  --role="roles/artifactregistry.reader"
```

#### 3. Create the Artifact Registry Repository (if not exists)
```bash
gcloud artifacts repositories create "${GAR_REPOSITORY}" \
  --repository-format=docker \
  --location="${GAR_LOCATION}" \
  --description="In-cluster storage subsystem images" \
  --project="${GCP_PROJECT}"
```

#### 4. Build and Push Container Images
Select one of the following paths to compile and register the container images.

##### Path A: Using Parallel Google Cloud Build (Recommended when local Docker is missing)
This path runs remote container compilation in GCP and does not require a local Docker daemon.

```bash
# Submit parallel cloud builds for all six storage subsystems
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
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  echo "=== Build log snapshot for ${img} ==="
  tail -n 10 "build-${img}.log" || true
  rm -f "build-${img}.log" "cloudbuild-${img}.yaml"
done
```

##### Path B: Using Local Docker Daemon (Requires local Docker)
```bash
# Build subsystem images
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  docker build -t "${REGISTRY}/${img}:${IMAGE_TAG}" -f "images/${img}/Dockerfile" .
  docker push "${REGISTRY}/${img}:${IMAGE_TAG}"
done
```

##### Path C: Using the Repository's Native `ap` Tool (Requires local Docker & Go)
```bash
# Build all subsystem images using native ap CLI
go run github.com/gke-labs/gke-labs-infra/ap@latest build //...

# Tag and push built ap images to GAR
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  docker tag "${img}:latest" "${REGISTRY}/${img}:${IMAGE_TAG}"
  docker push "${REGISTRY}/${img}:${IMAGE_TAG}"
done
```

#### 5. Update Manifests and Deploy Subsystems
Dynamically inject Artifact Registry images into the Kubernetes manifests and deploy them.

```bash
# Create temporary directory for updated manifests
mkdir -p build/manifests

# Replace local image references with remote GAR targets
sed -e "s|image: agentfs-controller:latest|image: ${REGISTRY}/agentfs-controller:${IMAGE_TAG}|g" \
    -e "s|image: agentfs-node-daemon:latest|image: ${REGISTRY}/agentfs-node-daemon:${IMAGE_TAG}|g" \
    k8s/manifest.yaml > build/manifests/manifest.yaml

sed -e "s|image: objectfs-controller:latest|image: ${REGISTRY}/objectfs-controller:${IMAGE_TAG}|g" \
    -e "s|image: objectfs-node-daemon:latest|image: ${REGISTRY}/objectfs-node-daemon:${IMAGE_TAG}|g" \
    k8s/objectfs.yaml > build/manifests/objectfs.yaml

sed -e "s|image: wal-buffer:latest|image: ${REGISTRY}/wal-buffer:${IMAGE_TAG}|g" \
    k8s/wal.yaml > build/manifests/wal.yaml

# Create target namespaces
kubectl create namespace kube-agentfs-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace kube-objectfs-system --dry-run=client -o yaml | kubectl apply -f -

# Apply subsystem manifests
kubectl apply -f build/manifests/manifest.yaml
kubectl apply -f build/manifests/wal.yaml
kubectl apply -f build/manifests/objectfs.yaml
```

##### (Optional) Deploy CAS CSI Driver
```bash
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
```

## Verify

Confirm healthy initialization of all deployed subsystems on the KOPS cluster.

### 1. Inspect Pod and CSI Statuses
```bash
# Verify CSI Drivers are registered on KOPS
kubectl get csidrivers | grep -E 'agentfs|objectfs|cas'

# Verify AgentFS, ObjectFS, and WAL Buffer pods are Running
kubectl get pods -n kube-agentfs-system -o wide
kubectl get pods -n kube-objectfs-system -o wide
```

### 2. Validate with Log Checks
```bash
# Verify controller starts and registers correctly
kubectl logs -n kube-agentfs-system statefulset/agentfs-controller -c agentfs-controller --tail=20
kubectl logs -n kube-objectfs-system statefulset/objectfs-controller -c objectfs-controller --tail=20

# Verify node-daemons can register CSI sockets and run without errors
kubectl logs -n kube-agentfs-system daemonset/agentfs-node-daemon -c agentfs-node-daemon --tail=20
```

## Teardown

To clean up resources and avoid incurring VM and storage costs:

### 1. Delete Kubernetes Resources
```bash
# Delete CAS
kubectl delete csidriver cas.labs.gke.io --ignore-not-found
kubectl delete daemonset cas-node-daemon -n kube-agentfs-system --ignore-not-found

# Delete ObjectFS and WAL Buffer
kubectl delete -f build/manifests/objectfs.yaml --ignore-not-found
kubectl delete -f build/manifests/wal.yaml --ignore-not-found
kubectl delete namespace kube-objectfs-system --ignore-not-found

# Delete AgentFS
kubectl delete -f build/manifests/manifest.yaml --ignore-not-found
kubectl delete namespace kube-agentfs-system --ignore-not-found

# Clean up local temporary manifests
rm -rf build/manifests
```

### 2. Destroy KOPS Cluster Resources
```bash
# Tear down the KOPS GCE resources
kops delete cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" --yes
```

### 3. Clean up GCS State Bucket and GAR Repository
```bash
# Delete KOPS GCS state bucket
gcloud storage buckets delete "${KOPS_STATE_STORE}" --quiet

# Delete Artifact Registry Repository
gcloud artifacts repositories delete "${GAR_REPOSITORY}" \
  --location="${GAR_LOCATION}" \
  --project="${GCP_PROJECT}" \
  --quiet
```
