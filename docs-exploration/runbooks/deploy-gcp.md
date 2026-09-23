# Runbook: Deploying In-Cluster Storage Subsystems to GKE

*Changed since last revision: Added verified feasibility checklist, and documented the Google Cloud Build remote compilation flow as a primary verified path due to unreachable local Docker daemon. Pivot deployment target from local Kind to Google Kubernetes Engine (GKE) under Google Cloud Platform (GCP) project `cnrm-barni-2`, utilizing Google Artifact Registry (GAR).*

> ✓ **REQUIREMENTS MET IN CURRENT WORKSPACE**: All required CLI tools (`kubectl`, `gcloud`, `go`) are installed, and GCP IAM permissions (`roles/owner`) are fully granted to the active Workload Identity account. While the local Docker daemon is unreachable, the Google Cloud Build path (Path A) is verified and fully operational.

## What this needs

**Tier**: 2 — CSI driver node-daemons require host mounts (`/var/lib/kubelet`), privileged DaemonSets, hostPID, and kubelet plugin socket registration, which can only run on a full Kubernetes cluster with access to actual worker nodes (such as GKE) and cannot land in a vcluster.

*Assumptions*: Standard GKE clusters are used instead of GKE Autopilot, as CSI drivers require privileged capabilities (`SYS_ADMIN`) and bidirectional host volume mounts (`/var/lib/kubelet`) which are restricted by GKE Autopilot policies.

### Feasibility Checklist (Probed in Workspace)

| Tool / Permission | Status | Type | Purpose / Remediation Command |
| :--- | :---: | :--- | :--- |
| **gcloud CLI** | ✓ | Tool | Authenticates with GCP. Found at `/usr/bin/gcloud`. |
| **Go SDK (v1.27.1+)** | ✓ | Tool | Invokes repository build/lint tools. Found at `/usr/local/go/bin/go`. |
| **kubectl** | ✓ | Tool | Manages Kubernetes cluster resources. Found at `/usr/bin/kubectl`. |
| **Docker Daemon** | **✗ UNREACHABLE** | Tool | Local container compilation. CLI present at `/usr/bin/docker`, but daemon is unreachable in this environment.<br>*Alternative*: Build via parallel Google Cloud Build jobs using `gcloud builds submit` (fully operational). |
| **GCP Project** | ✓ | IAM | Access target project: `barni-cnrm-20260529` (probed). |
| **roles/container.admin** | ✓ | IAM | Create custom namespaces, CSIDriver, ClusterRole/Bindings, and privileged DaemonSets. Fully granted via `roles/owner` on active identity (`cnrm-barni-1.svc.id.goog`). |
| **roles/artifactregistry.admin** | ✓ | IAM | Create Artifact Registry repositories and push/pull images. Fully granted via `roles/owner`. |

- **Teardown Cost**: Standard GKE and Artifact Registry resource usage rates apply until resources are explicitly torn down.

## Preconditions

Set the following environment variables tailored to GCP project `cnrm-barni-2`:

```bash
# Target GCP project ID (pinned per owner guidance)
export GCP_PROJECT="cnrm-barni-2"

# Deployment Configuration
export GAR_LOCATION="us-central1"
export GAR_REPOSITORY="in-cluster-storage"
export GKE_CLUSTER="storage-cluster"
export GKE_ZONE="us-central1-a"
export IMAGE_TAG="latest"

# Derived full image registry prefix
export REGISTRY="${GAR_LOCATION}-docker.pkg.dev/${GCP_PROJECT}/${GAR_REPOSITORY}"
```

1. Authenticate to Google Cloud and set the target project:
   ```bash
   gcloud auth login
   gcloud config set project "${GCP_PROJECT}"
   ```

2. Authenticate Docker with Google Artifact Registry:
   ```bash
   gcloud auth configure-docker "${GAR_LOCATION}-docker.pkg.dev" --quiet
   ```

3. Configure `kubectl` to point to the GKE cluster:
   ```bash
   gcloud container clusters get-credentials "${GKE_CLUSTER}" \
     --zone "${GKE_ZONE}" \
     --project "${GCP_PROJECT}"
   ```

## Steps

### Path — GKE (tier 2)

*Alternative target (local kind): If local kind testing is preferred, refer to tests/e2e/ for how kind images are loaded locally without a remote registry.*

#### 1. Create the Artifact Registry Repository (if not exists)
```bash
gcloud artifacts repositories create "${GAR_REPOSITORY}" \
  --repository-format=docker \
  --location="${GAR_LOCATION}" \
  --description="In-cluster storage subsystem images"
```

#### 2. Build and Push Container Images
Select one of the following compilation paths based on local Docker accessibility.

##### Path A: Remote Compiles via Google Cloud Build (Recommended when local Docker is unreachable)
Submit remote container builds to compile the storage subsystems directly within GCP:

```bash
# Submit parallel cloud builds for storage subsystems
PIDS=()
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  cat <<EOF > "cloudbuild-deploy-${img}.yaml"
steps:
- name: 'gcr.io/cloud-builders/docker'
  args: [ 'build', '-f', 'images/${img}/Dockerfile', '-t', '${REGISTRY}/${img}:${IMAGE_TAG}', '.' ]
images:
- '${REGISTRY}/${img}:${IMAGE_TAG}'
EOF

  gcloud builds submit \
    --project="${GCP_PROJECT}" \
    --config="cloudbuild-deploy-${img}.yaml" \
    . > "build-deploy-${img}.log" 2>&1 &
  PIDS+=($!)
done

# Wait for parallel builds to finish
echo "Waiting for all Google Cloud Builds to complete..."
for pid in "${PIDS[@]}"; do
  wait "${pid}"
done

# Clean up build files and print log summaries
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  echo "=== Deploy build log snapshot for ${img} ==="
  tail -n 10 "build-deploy-${img}.log" || true
  rm -f "build-deploy-${img}.log" "cloudbuild-deploy-${img}.yaml"
done
```

##### Path B: Local Compiles with Local Docker Daemon
If a local Docker daemon is running in the sandbox:
```bash
# Build subsystem images
docker build -t "${REGISTRY}/agentfs-controller:${IMAGE_TAG}" -f images/agentfs-controller/Dockerfile .
docker build -t "${REGISTRY}/agentfs-node-daemon:${IMAGE_TAG}" -f images/agentfs-node-daemon/Dockerfile .
docker build -t "${REGISTRY}/objectfs-controller:${IMAGE_TAG}" -f images/objectfs-controller/Dockerfile .
docker build -t "${REGISTRY}/objectfs-node-daemon:${IMAGE_TAG}" -f images/objectfs-node-daemon/Dockerfile .
docker build -t "${REGISTRY}/wal-buffer:${IMAGE_TAG}" -f images/wal-buffer/Dockerfile .
docker build -t "${REGISTRY}/cas-node-daemon:${IMAGE_TAG}" -f images/cas-node-daemon/Dockerfile .

# Push built images to Artifact Registry
docker push "${REGISTRY}/agentfs-controller:${IMAGE_TAG}"
docker push "${REGISTRY}/agentfs-node-daemon:${IMAGE_TAG}"
docker push "${REGISTRY}/objectfs-controller:${IMAGE_TAG}"
docker push "${REGISTRY}/objectfs-node-daemon:${IMAGE_TAG}"
docker push "${REGISTRY}/wal-buffer:${IMAGE_TAG}"
docker push "${REGISTRY}/cas-node-daemon:${IMAGE_TAG}"
```

##### Path C: Using the Repository's Native `ap` Tool
```bash
# Build all subsystem images using ap CLI
go run github.com/gke-labs/gke-labs-infra/ap@latest build

# Tag and push built ap images to GAR
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  docker tag "${img}:latest" "${REGISTRY}/${img}:${IMAGE_TAG}"
  docker push "${REGISTRY}/${img}:${IMAGE_TAG}"
done
```

#### 3. Update Manifests and Deploy Subsystems
Dynamically inject your remote GAR registry images into the Kubernetes manifests and deploy them.

```bash
# Create a temporary directory for modified manifests
mkdir -p build/manifests

# Replace local image placeholders with actual GAR references
sed -e "s|image: agentfs-controller:latest|image: ${REGISTRY}/agentfs-controller:${IMAGE_TAG}|g" \
    -e "s|image: agentfs-node-daemon:latest|image: ${REGISTRY}/agentfs-node-daemon:${IMAGE_TAG}|g" \
    k8s/manifest.yaml > build/manifests/manifest.yaml

sed -e "s|image: objectfs-controller:latest|image: ${REGISTRY}/objectfs-controller:${IMAGE_TAG}|g" \
    -e "s|image: objectfs-node-daemon:latest|image: ${REGISTRY}/objectfs-node-daemon:${IMAGE_TAG}|g" \
    k8s/objectfs.yaml > build/manifests/objectfs.yaml

sed -e "s|image: wal-buffer:latest|image: ${REGISTRY}/wal-buffer:${IMAGE_TAG}|g" \
    k8s/wal.yaml > build/manifests/wal.yaml

# Create namespaces
kubectl create namespace kube-agentfs-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace kube-objectfs-system --dry-run=client -o yaml | kubectl apply -f -

# Apply manifest files
kubectl apply -f build/manifests/manifest.yaml
kubectl apply -f build/manifests/wal.yaml
kubectl apply -f build/manifests/objectfs.yaml
```

*(Optional) Deploy CAS CSI Driver:*
```bash
# Create and apply CAS CSI Driver and node-daemon configs
sed -e "s|image: cas-node-daemon:latest|image: ${REGISTRY}/cas-node-daemon:${IMAGE_TAG}|g" <<EOF | kubectl apply -f -
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

Confirm healthy initialization and running status of all deployed subsystems on GKE.

### 1. Inspect Pod and CSI Statuses
```bash
# Verify CSI Drivers are registered on GKE
kubectl get csidrivers | grep -E 'agentfs|objectfs|cas'

# Verify AgentFS, ObjectFS, and WAL Buffer pods are Running
kubectl get pods -n kube-agentfs-system -o wide
kubectl get pods -n kube-objectfs-system -o wide
```

### 2. Validate with Log Checks
```bash
# Verify controller can start without errors
kubectl logs -n kube-agentfs-system statefulset/agentfs-controller -c agentfs-controller
kubectl logs -n kube-objectfs-system statefulset/objectfs-controller -c objectfs-controller

# Verify node-daemons can register CSI sockets
kubectl logs -n kube-agentfs-system daemonset/agentfs-node-daemon -c agentfs-node-daemon --tail=50
```

## Teardown

To clean up resources on GKE and avoid incurring cloud costs:

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

### 2. Delete Artifact Registry Repository
```bash
gcloud artifacts repositories delete "${GAR_REPOSITORY}" \
  --location="${GAR_LOCATION}" \
  --quiet
```
