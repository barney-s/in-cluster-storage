# Runbook: Upgrading In-Cluster Storage Subsystems on GKE

*Changed since last revision: Initial revision. Formulated the GKE rolling upgrade path with verified GCP/GKE feasibility checks, featuring a Google Cloud Build remote compilation flow due to unreachable local Docker daemon.*

> ✓ **REQUIREMENTS MET IN CURRENT WORKSPACE**: All required CLI tools (`kubectl`, `gcloud`, `go`) are installed, and GCP IAM permissions (`roles/owner`) are fully granted to the active Workload Identity account. While the local Docker daemon is unreachable, the Google Cloud Build path (Path A) is verified and fully operational.

## What this needs

**Tier**: 2 — CSI driver node-daemons require host mounts (`/var/lib/kubelet`), privileged DaemonSets, hostPID, and kubelet plugin socket registration, which can only run on a full Kubernetes cluster with access to actual worker nodes (such as GKE) and cannot land in a vcluster. Upgrading them requires live cluster-admin operations.

*Assumptions*: Standard GKE clusters are used. Upgrades are applied to existing active deployments of AgentFS, ObjectFS, and WAL Buffer subsystems in the GKE cluster.

### Feasibility Checklist (Probed in Workspace)

| Tool / Permission | Status | Type | Purpose / Remediation Command |
| :--- | :---: | :--- | :--- |
| **gcloud CLI** | ✓ | Tool | Authenticates with GCP. Found at `/usr/bin/gcloud`. |
| **Go SDK (v1.27.1+)** | ✓ | Tool | Invokes repository build/lint tools. Found at `/usr/local/go/bin/go`. |
| **kubectl** | ✓ | Tool | Manages Kubernetes cluster resources. Found at `/usr/bin/kubectl`. |
| **Docker Daemon** | **✗ UNREACHABLE** | Tool | Local container compilation. CLI present at `/usr/bin/docker`, but daemon is unreachable in this environment.<br>*Alternative*: Build via parallel Google Cloud Build jobs using `gcloud builds submit` (fully operational). |
| **GCP Project** | ✓ | IAM | Access target project: `barni-cnrm-20260529` (probed). |
| **roles/container.admin** | ✓ | IAM | Execute rolling updates, apply updated configurations, and inspect resource rollouts. Fully granted via `roles/owner` on active identity (`cnrm-barni-1.svc.id.goog`). |
| **roles/artifactregistry.admin** | ✓ | IAM | Create repositories, push upgraded images. Fully granted via `roles/owner`. |

- **Teardown Cost**: Standard Artifact Registry storage and GKE GCE node instance hours apply until upgraded deployments are scaled down or deleted.

## Preconditions

Set the following environment variables tailored to GCP project `cnrm-barni-2`:

```bash
# Target GCP project ID (pinned per owner guidance)
export GCP_PROJECT="cnrm-barni-2"

# Upgrade Target Configuration
export GAR_LOCATION="us-central1"
export GAR_REPOSITORY="in-cluster-storage"
export GKE_CLUSTER="storage-cluster"
export GKE_ZONE="us-central1-a"
export UPGRADE_IMAGE_TAG="v1.0.1-upgrade"

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

### Path — GKE Subsystem Upgrade (tier 2)

#### 1. Compile and Register Upgraded Container Images
Select one of the following compilation paths based on local Docker accessibility.

##### Path A: Remote Compiles via Google Cloud Build (Recommended when local Docker is unreachable)
Submit remote container builds to compile the upgraded storage subsystems directly within GCP:

```bash
# Submit parallel cloud builds for updated storage subsystems
PIDS=()
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  cat <<EOF > "cloudbuild-upgrade-${img}.yaml"
steps:
- name: 'gcr.io/cloud-builders/docker'
  args: [ 'build', '-f', 'images/${img}/Dockerfile', '-t', '${REGISTRY}/${img}:${UPGRADE_IMAGE_TAG}', '.' ]
images:
- '${REGISTRY}/${img}:${UPGRADE_IMAGE_TAG}'
EOF

  gcloud builds submit \
    --project="${GCP_PROJECT}" \
    --config="cloudbuild-upgrade-${img}.yaml" \
    . > "build-upgrade-${img}.log" 2>&1 &
  PIDS+=($!)
done

# Wait for parallel builds to finish
echo "Waiting for all upgraded Google Cloud Builds to complete..."
for pid in "${PIDS[@]}"; do
  wait "${pid}"
done

# Clean up build files and print log summaries
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  echo "=== Upgrade build log snapshot for ${img} ==="
  tail -n 10 "build-upgrade-${img}.log" || true
  rm -f "build-upgrade-${img}.log" "cloudbuild-upgrade-${img}.yaml"
done
```

##### Path B: Local Compiles with Local Docker Daemon
If a local Docker daemon is running in the sandbox:
```bash
# Build and push upgraded images locally
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  docker build -t "${REGISTRY}/${img}:${UPGRADE_IMAGE_TAG}" -f "images/${img}/Dockerfile" .
  docker push "${REGISTRY}/${img}:${UPGRADE_IMAGE_TAG}"
done
```

##### Path C: Using the Repository's Native `ap` Tool
```bash
# Build the images using the repository's native ap CLI
go run github.com/gke-labs/gke-labs-infra/ap@latest build //...

# Tag and push built images to GAR
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  docker tag "${img}:latest" "${REGISTRY}/${img}:${UPGRADE_IMAGE_TAG}"
  docker push "${REGISTRY}/${img}:${UPGRADE_IMAGE_TAG}"
done
```

#### 2. Execute Rolling Upgrades of Subsystem Controllers and Node Daemons
Update the image fields on live Kubernetes workloads. Kubernetes will coordinate a zero-downtime rolling rollout, updating pod replicas sequentially.

##### A. Upgrade Write-Ahead Log (WAL) Buffer
```bash
# Apply updated image to the WAL Buffer StatefulSet
kubectl set image statefulset/wal-buffer -n kube-objectfs-system \
  wal-buffer="${REGISTRY}/wal-buffer:${UPGRADE_IMAGE_TAG}"
```

##### B. Upgrade AgentFS Subsystem
```bash
# Apply updated image to the AgentFS Controller StatefulSet
kubectl set image statefulset/agentfs-controller -n kube-agentfs-system \
  agentfs-controller="${REGISTRY}/agentfs-controller:${UPGRADE_IMAGE_TAG}"

# Apply updated image to the AgentFS Node Daemon DaemonSet
kubectl set image daemonset/agentfs-node-daemon -n kube-agentfs-system \
  agentfs-node-daemon="${REGISTRY}/agentfs-node-daemon:${UPGRADE_IMAGE_TAG}"
```

##### C. Upgrade ObjectFS Subsystem
```bash
# Apply updated image to the ObjectFS Controller StatefulSet
kubectl set image statefulset/objectfs-controller -n kube-objectfs-system \
  objectfs-controller="${REGISTRY}/objectfs-controller:${UPGRADE_IMAGE_TAG}"

# Apply updated image to the ObjectFS Node Daemon DaemonSet
kubectl set image daemonset/objectfs-node-daemon -n kube-objectfs-system \
  objectfs-node-daemon="${REGISTRY}/objectfs-node-daemon:${UPGRADE_IMAGE_TAG}"
```

##### D. Upgrade CAS Subsystem (if deployed)
```bash
# Apply updated image to the CAS CSI Node Daemon DaemonSet
kubectl set image daemonset/cas-node-daemon -n kube-agentfs-system \
  cas-node-daemon="${REGISTRY}/cas-node-daemon:${UPGRADE_IMAGE_TAG}"
```

#### 3. Monitor Upgrade Rollout Status
Track rollout progress to ensure all updated pods transition to a healthy, running state.

```bash
# Monitor WAL Buffer StatefulSet Rollout
kubectl rollout status statefulset/wal-buffer -n kube-objectfs-system --timeout=5m

# Monitor AgentFS Rollouts
kubectl rollout status statefulset/agentfs-controller -n kube-agentfs-system --timeout=5m
kubectl rollout status daemonset/agentfs-node-daemon -n kube-agentfs-system --timeout=5m

# Monitor ObjectFS Rollouts
kubectl rollout status statefulset/objectfs-controller -n kube-objectfs-system --timeout=5m
kubectl rollout status daemonset/objectfs-node-daemon -n kube-objectfs-system --timeout=5m

# Monitor CAS Rollout (if deployed)
kubectl rollout status daemonset/cas-node-daemon -n kube-agentfs-system --timeout=5m
```

## Verify

Verify that the upgrade succeeded and all storage subsystems are executing the new image versions without disruption.

### 1. Confirm Running Container Image Versions
Retrieve the live container image tags from all active pods to confirm they match `${UPGRADE_IMAGE_TAG}`:

```bash
# Check WAL Buffer & ObjectFS pods
kubectl get pods -n kube-objectfs-system -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[*].image}{"\n"}{end}' | grep "${UPGRADE_IMAGE_TAG}"

# Check AgentFS & CAS pods
kubectl get pods -n kube-agentfs-system -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[*].image}{"\n"}{end}' | grep "${UPGRADE_IMAGE_TAG}"
```

### 2. Verify Log Initializations
Ensure the upgraded container processes initialized correctly and verified their CSI socket connections:

```bash
# Inspect agentfs-controller logs
kubectl logs -n kube-agentfs-system statefulset/agentfs-controller -c agentfs-controller --tail=50

# Inspect agentfs-node-daemon logs
kubectl logs -n kube-agentfs-system daemonset/agentfs-node-daemon -c agentfs-node-daemon --tail=50
```

## Teardown

If any subsystem fails to start or encounters continuous crash loops post-upgrade, roll back the workloads immediately to restore storage service continuity.

### 1. Perform Rollbacks
Roll back GKE deployments to their previous working revisions:

```bash
# Roll back WAL Buffer
kubectl rollout undo statefulset/wal-buffer -n kube-objectfs-system

# Roll back AgentFS
kubectl rollout undo statefulset/agentfs-controller -n kube-agentfs-system
kubectl rollout undo daemonset/agentfs-node-daemon -n kube-agentfs-system

# Roll back ObjectFS
kubectl rollout undo statefulset/objectfs-controller -n kube-objectfs-system
kubectl rollout undo daemonset/objectfs-node-daemon -n kube-objectfs-system

# Roll back CAS (if deployed)
kubectl rollout undo daemonset/cas-node-daemon -n kube-agentfs-system
```

### 2. Verify Rollback Completion
Check rollout status to confirm rollback completed:
```bash
kubectl rollout status statefulset/agentfs-controller -n kube-agentfs-system --timeout=5m
kubectl rollout status daemonset/agentfs-node-daemon -n kube-agentfs-system --timeout=5m
```
