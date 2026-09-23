# KOPS GCE Deployment Runbook

*Revision Note: Updated Section 3 (Provision the KOPS Kubernetes Cluster) to sequence `kops create` (creating configuration manifests in GCS and exporting them to the local directory), `kops update` (actually provisioning resources in GCE), and `kops validate` for proper lifecycle management.*

*(Pinned)* **This runbook is executable top-to-bottom on GCE.** It builds, deploys, verifies, and tears down the four core storage subsystems (`agentfs`, `objectfs`, `wal-buffer`, and `cas`) on a Kubernetes cluster managed by KOPS on Google Compute Engine (GCE).

---

## What this needs

### Deployment Type & Infrastructure Requirement
This runbook **requires real Google Compute Engine (GCE) infrastructure**. It cannot run in a local pod sandbox (`in-pod`) or standard non-privileged environment.
*   **Reason**: The storage subsystems run as low-level Kubernetes CSI drivers (`agentfs.labs.gke.io`, `objectfs.labs.gke.io`, `cas.labs.gke.io`) and write-ahead log daemons (`wal-buffer`). CSI node-daemons operate as privileged DaemonSets that interact directly with the Linux kernel (via `fanotify` and `SYS_ADMIN` capability) and require direct bidirectional volume mount propagation on host paths (`/var/lib/kubelet` and `/var/cache/cas`). This level of kernel and host integration is impossible to replicate "in-pod".

### Enumed IAM Permissions
To execute this runbook, the calling Google Cloud identity must have the following IAM permissions on the target GCP project:
1.  **Resource Manager Permissions**:
    *   `resourcemanager.projects.get` & `resourcemanager.projects.getIamPolicy`
    *   `resourcemanager.projects.setIamPolicy` (needed by KOPS to bind service accounts, and to grant Artifact Registry access to node SAs)
2.  **Compute Engine Permissions**:
    *   Full instance and network provisioning capabilities (`roles/compute.admin` or `roles/editor`) to spawn cluster nodes
3.  **Storage Permissions**:
    *   `storage.buckets.create` & `storage.buckets.update` (for the KOPS state store bucket)
    *   `storage.buckets.setIamPolicy` (to grant bucket access to KOPS/Active principal)
4.  **Artifact Registry Permissions**:
    *   `artifactregistry.repositories.create`
    *   `artifactregistry.repositories.get`
    *   `artifactregistry.repositories.uploadArtifacts` (to push built images)
5.  **Cloud Build Permissions**:
    *   `cloudbuild.builds.create` (if submitting parallel remote builds)

### Verified Feasibility Checklist
*   ✓ **`gcloud` CLI**: Present (Google Cloud SDK installed).
*   ✓ **`kops` CLI**: Present (KOPS cluster manager installed).
*   ✓ **`kubectl` CLI**: Present.
*   ✓ **`docker` CLI**: Present.
*   ✓ **Active GCP Credential**: Granted `roles/owner` or `roles/editor` on the active project `barni-cnrm-20260529`.

---

## Preconditions

Ensure the following instance variables are resolved and exported in your shell. You can source them from a local `params.env`:

```bash
export GCP_PROJECT="barni-cnrm-20260529" # Replace with your project ID
export GCP_REGION="us-central1"
export GCP_ZONE="us-central1-a"

export CLUSTER="ics3"
export KOPS_CLUSTER_NAME="${CLUSTER}.k8s.local"
export KOPS_STATE_STORE="gs://${GCP_PROJECT}-${CLUSTER}-kops-state"

export GAR_LOCATION="${GCP_REGION}"
export GAR_REPOSITORY="in-cluster-storage"
export IMAGE_TAG="latest"

export REGISTRY="${GAR_LOCATION}-docker.pkg.dev/${GCP_PROJECT}/${GAR_REPOSITORY}"
```

---

## Steps

### 1. Set Project & Configure Docker Authentication
Set the active Google Cloud project and configure Docker to authenticate with Google Artifact Registry:
```bash
gcloud config set project "${GCP_PROJECT}"
gcloud auth configure-docker "${GAR_LOCATION}-docker.pkg.dev" --quiet
```

### 2. Prepare the KOPS GCS State Store Bucket
KOPS stores cluster specifications and state in GCS. Standard sandbox environments enforce **Uniform Bucket Level Access (UBLA)**. Create the bucket with UBLA enabled and grant `storage.admin` to the executing identity:
```bash
if ! gcloud storage buckets describe "${KOPS_STATE_STORE}" >/dev/null 2>&1; then
  gcloud storage buckets create "${KOPS_STATE_STORE}" --project="${GCP_PROJECT}" --location="${GCP_REGION}" --uniform-bucket-level-access
else
  gcloud storage buckets update "${KOPS_STATE_STORE}" --uniform-bucket-level-access
fi

# Dynamically grant storage.admin to the active workload identity principal
ACTIVE_PRINCIPAL=$(gcloud projects get-iam-policy "${GCP_PROJECT}" --format="value(bindings.members)" | tr -d "['\",]" | grep -o 'principal://[^ ]*' | sort -u | head -n 1 || true)
if [ -n "${ACTIVE_PRINCIPAL}" ]; then
  gcloud storage buckets add-iam-policy-binding "${KOPS_STATE_STORE}" \
    --member="${ACTIVE_PRINCIPAL}" \
    --role="roles/storage.admin" \
    --quiet
fi
```

### 3. Provision the KOPS Kubernetes Cluster
Generate the kOps cluster configuration manifests in the state store, export them to a local directory so they can be checked into git, then actually provision the VMs and network infrastructure on GCE. This provisions 1 Master (Control Plane) and 3 Workers of type `e2-standard-2`:
```bash
if ! kops get cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" >/dev/null 2>&1; then
  # 1. Create the cluster definition (manifests) inside the GCS state store
  kops create cluster \
    --name="${KOPS_CLUSTER_NAME}" \
    --zones="${GCP_ZONE}" \
    --state="${KOPS_STATE_STORE}" \
    --project="${GCP_PROJECT}" \
    --cloud=gce \
    --node-count=3 \
    --node-size=e2-standard-2 \
    --master-size=e2-standard-2

  # 2. Export the generated cluster configuration manifests so they can be checked into git
  echo "Exporting generated kOps configuration manifests to the deployment directory..."
  mkdir -p docs-exploration/runbook-deployments/ics3/manifests
  kops get cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" -o yaml > docs-exploration/runbook-deployments/ics3/manifests/cluster.yaml
  kops get ig --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" -o yaml >> docs-exploration/runbook-deployments/ics3/manifests/cluster.yaml
fi

# 3. Apply the manifests and actually provision the cloud infrastructure resources (VMs, network, etc.)
kops update cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" --yes

# 4. Validate cluster rollout (Wait up to 10 minutes for virtual machines to configure and register)
kops validate cluster --state="${KOPS_STATE_STORE}" --wait 10m
```

### 4. Authorize Nodes to Pull Private Container Images
KOPS-managed GCE nodes use dedicated Service Accounts. You must grant them `roles/artifactregistry.reader` so they can pull private images without requiring local secrets:
```bash
# Authorize default compute SA
COMPUTE_SVC_ACCT=$(gcloud iam service-accounts list --filter="displayName:Compute Engine default service account" --format="value(email)" --project="${GCP_PROJECT}")
if [ -n "${COMPUTE_SVC_ACCT}" ]; then
  gcloud projects add-iam-policy-binding "${GCP_PROJECT}" \
    --member="serviceAccount:${COMPUTE_SVC_ACCT}" \
    --role="roles/artifactregistry.reader" \
    --quiet || true
fi

# Authorize dedicated KOPS Node Service Account
KOPS_NODE_SA="node-${KOPS_CLUSTER_NAME//./-}@${GCP_PROJECT}.iam.gserviceaccount.com"
gcloud projects add-iam-policy-binding "${GCP_PROJECT}" \
  --member="serviceAccount:${KOPS_NODE_SA}" \
  --role="roles/artifactregistry.reader" \
  --quiet || true

# Authorize dedicated KOPS Control-Plane Service Account
KOPS_CP_SA="control-plane-${KOPS_CLUSTER_NAME//./-}@${GCP_PROJECT}.iam.gserviceaccount.com"
gcloud projects add-iam-policy-binding "${GCP_PROJECT}" \
  --member="serviceAccount:${KOPS_CP_SA}" \
  --role="roles/artifactregistry.reader" \
  --quiet || true
```

### 5. Create Artifact Registry Repository
Create the GAR repository to store the built subsystem images:
```bash
if ! gcloud artifacts repositories describe "${GAR_REPOSITORY}" --location="${GAR_LOCATION}" --project="${GCP_PROJECT}" >/dev/null 2>&1; then
  gcloud artifacts repositories create "${GAR_REPOSITORY}" \
    --repository-format=docker \
    --location="${GAR_LOCATION}" \
    --description="In-cluster storage subsystem images" \
    --project="${GCP_PROJECT}"
fi
```

### 6. Build and Push Subsystem Images via Parallel Cloud Build
We submit parallel Cloud Builds to compile and push all 6 target images. This minimizes local machine compute consumption and utilizes high-speed remote build infrastructure:
```bash
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

# Wait for parallel compilation jobs to complete
for pid in "${PIDS[@]}"; do
  wait "${pid}"
done

# Clean up build manifests and temporary files
rm -f build-*.log cloudbuild-*.yaml
```

### 7. Compile Manifests and Deploy Subsystems
Inject your custom registry path into the standard manifests and apply them:
```bash
mkdir -p build/manifests

# Sub out image names for AgentFS, ObjectFS, and WAL
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

# Deploy storage subsystems
kubectl apply -f build/manifests/manifest.yaml
kubectl apply -f build/manifests/wal.yaml
kubectl apply -f build/manifests/objectfs.yaml
```

### 8. Deploy Content Addressable Storage (CAS) CSI Driver
Deploy the CAS CSIDriver object and its accompanying node daemon DaemonSet:
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

---

## Verify

Execute the following commands to confirm that the deployment is fully operational:

### 1. Check CSI Driver Registration
Ensure the custom CSI drivers are registered in the cluster:
```bash
kubectl get csidrivers | grep -E 'agentfs|objectfs|cas'
```
*Expected Output*:
```
agentfs.labs.gke.io     false            true             false             <unset>         false               Ephemeral
cas.labs.gke.io         false            true             false             <unset>         false               Ephemeral
objectfs.labs.gke.io    false            true             false             <unset>         false               Ephemeral,Persistent
```

### 2. Verify Workload Pod Statuses
All pods must be in `Running` state and show healthy statuses (no crashes or restarts):
```bash
kubectl get pods -n kube-agentfs-system -o wide
kubectl get pods -n kube-objectfs-system -o wide
```
*Expected Output*:
*   `kube-agentfs-system`: `agentfs-controller-0` (1/1 Running), `agentfs-node-daemon-xxx` (2/2 Running), and `cas-node-daemon-xxx` (2/2 Running).
*   `kube-objectfs-system`: `objectfs-controller-0` (2/2 Running), `objectfs-node-daemon-xxx` (2/2 Running), and `wal-buffer-0` (1/1 Running).

### 3. Check Core Components Connection Logs
Verify that controllers are listening and drivers are connected:
```bash
kubectl logs -n kube-agentfs-system statefulset/agentfs-controller -c agentfs-controller | grep -i "listening"
kubectl logs -n kube-objectfs-system statefulset/objectfs-controller -c objectfs-controller | grep -i "listening"
```

---

## Teardown

To destroy the cluster and clean up all resources, execute the following commands in order:

### 1. Delete Subsystem Workloads & Namespaces
```bash
kubectl delete csidriver cas.labs.gke.io --ignore-not-found
kubectl delete daemonset cas-node-daemon -n kube-agentfs-system --ignore-not-found

kubectl delete -f build/manifests/objectfs.yaml --ignore-not-found
kubectl delete -f build/manifests/wal.yaml --ignore-not-found
kubectl delete namespace kube-objectfs-system --ignore-not-found

kubectl delete -f build/manifests/manifest.yaml --ignore-not-found
kubectl delete namespace kube-agentfs-system --ignore-not-found

rm -rf build/manifests
```

### 2. Teardown KOPS Infrastructure
Delete all cloud virtual machines, firewalls, disks, and VPCs provisioned by KOPS on GCE:
```bash
kops delete cluster --name="${KOPS_CLUSTER_NAME}" --state="${KOPS_STATE_STORE}" --yes
```

### 3. Clean Up Cloud Buckets and Container Registry
Delete the state GCS bucket and Artifact Registry repositories to stop billing:
```bash
gcloud storage buckets delete "${KOPS_STATE_STORE}" --quiet
gcloud artifacts repositories delete "${GAR_REPOSITORY}" --location="${GAR_LOCATION}" --project="${GCP_PROJECT}" --quiet
```
