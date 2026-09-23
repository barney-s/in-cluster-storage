# GKE Deployment Runbook

*(Pinned)* **This runbook is executable top-to-bottom on GKE.** It builds, deploys, verifies, and tears down the four core storage subsystems (`agentfs`, `objectfs`, `wal-buffer`, and `cas`) on Google Kubernetes Engine (GKE).

---

## What this needs

### Deployment Type & Infrastructure Requirement
This runbook **requires real Google Kubernetes Engine (GKE) infrastructure**. It cannot run in a local pod sandbox (`in-pod`) or standard non-privileged environment.
*   **Reason**: The storage subsystems are low-level Kubernetes CSI drivers (`agentfs.labs.gke.io`, `objectfs.labs.gke.io`, `cas.labs.gke.io`) and write-ahead log daemons (`wal-buffer`). These run as privileged DaemonSets that interact directly with the Linux kernel (using `fanotify` and `SYS_ADMIN` capability) and require direct bidirectional volume mount propagation on physical host paths (`/var/lib/kubelet` and `/var/cache/cas`). This level of node host integration is not possible in simulated "in-pod" envs.
*   **GKE Flavor Constraint**: You **must use GKE Standard**. GKE Autopilot restricts custom CSI drivers, direct physical host mounts, and raw privileged container execution for security reasons, making GKE Standard a strict prerequisite.

### Enumed IAM Permissions
To execute this runbook, the calling Google Cloud identity must have the following IAM permissions on the target GCP project:
1.  **GKE Cluster Permissions**:
    *   `container.clusters.create`
    *   `container.clusters.get` & `container.clusters.list`
    *   `container.clusters.update`
2.  **Artifact Registry Permissions**:
    *   `artifactregistry.repositories.create`
    *   `artifactregistry.repositories.get`
    *   `artifactregistry.repositories.uploadArtifacts` (to upload compiled images)
3.  **Cloud Build Permissions**:
    *   `cloudbuild.builds.create` (to execute high-speed remote parallel builds)
4.  **Kubernetes RBAC Permissions**:
    *   Full cluster admin access to create namespaces, CSIDrivers, DaemonSets, StatefulSets, ClusterRoles, and ClusterRoleBindings.

### Verified Feasibility Checklist
*   ✓ **`gcloud` CLI**: Present (Google Cloud SDK installed).
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

export CLUSTER_NAME="ics-gke-cluster"
export GAR_LOCATION="${GCP_REGION}"
export GAR_REPOSITORY="in-cluster-storage"
export IMAGE_TAG="latest"

export REGISTRY="${GAR_LOCATION}-docker.pkg.dev/${GCP_PROJECT}/${GAR_REPOSITORY}"
```

---

## Steps

### 1. Set Project & Configure Docker Authentication
Configure the active Google Cloud project and authenticate your local Docker client to pull/push from Google Artifact Registry:
```bash
gcloud config set project "${GCP_PROJECT}"
gcloud auth configure-docker "${GAR_LOCATION}-docker.pkg.dev" --quiet
```

### 2. Provision GKE Standard Cluster
Create a GKE Standard cluster configured with 3 nodes of type `e2-standard-4` (or similar) to provide sufficient compute capacity for the DaemonSets and workload tests:
```bash
if ! gcloud container clusters describe "${CLUSTER_NAME}" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" >/dev/null 2>&1; then
  gcloud container clusters create "${CLUSTER_NAME}" \
    --zone="${GCP_ZONE}" \
    --project="${GCP_PROJECT}" \
    --num-nodes=3 \
    --machine-type="e2-standard-4" \
    --enable-ip-alias \
    --quiet
fi

# Connect kubectl to the newly provisioned GKE cluster
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --zone="${GCP_ZONE}" \
  --project="${GCP_PROJECT}"
```

### 3. Create Artifact Registry Repository
Create the private registry repository if it does not already exist:
```bash
if ! gcloud artifacts repositories describe "${GAR_REPOSITORY}" --location="${GAR_LOCATION}" --project="${GCP_PROJECT}" >/dev/null 2>&1; then
  gcloud artifacts repositories create "${GAR_REPOSITORY}" \
    --repository-format=docker \
    --location="${GAR_LOCATION}" \
    --description="In-cluster storage subsystem images" \
    --project="${GCP_PROJECT}"
fi
```

### 4. Build and Push Subsystem Images via Parallel Cloud Build
Submit 6 concurrent Google Cloud Build jobs to compile the Go binaries and build/push their container images. This leverages GCP infrastructure for fast compilation and avoids local Docker daemon overhead:
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

# Wait for all remote parallel builds to complete
for pid in "${PIDS[@]}"; do
  wait "${pid}"
done

# Clean up build manifests and log files
rm -f build-*.log cloudbuild-*.yaml
```

### 5. Compile Manifests and Deploy Subsystems
Substitute the image placeholders in the repository manifests with the newly compiled GAR image tags:
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

# Create core namespaces
kubectl create namespace kube-agentfs-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace kube-objectfs-system --dry-run=client -o yaml | kubectl apply -f -

# Apply manifests to GKE
kubectl apply -f build/manifests/manifest.yaml
kubectl apply -f build/manifests/wal.yaml
kubectl apply -f build/manifests/objectfs.yaml
```

### 6. Deploy Content Addressable Storage (CAS) CSI Driver
Apply the CAS driver spec and deploy the node daemon DaemonSet:
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

Ensure that all deployed workloads run correctly on GKE:

### 1. Check Registered CSI Drivers
Ensure GKE recognizes the custom storage providers:
```bash
kubectl get csidrivers | grep -E 'agentfs|objectfs|cas'
```
*Expected Output*:
```
agentfs.labs.gke.io     false            true             false             <unset>         false               Ephemeral
cas.labs.gke.io         false            true             false             <unset>         false               Ephemeral
objectfs.labs.gke.io    false            true             false             <unset>         false               Ephemeral,Persistent
```

### 2. Verify Subsystem Pod Health
Check that all controllers, node-daemons, and buffers are up and Running on GKE:
```bash
kubectl get pods -n kube-agentfs-system -o wide
kubectl get pods -n kube-objectfs-system -o wide
```
*Expected Output*:
*   `kube-agentfs-system`: `agentfs-controller-0` (1/1 Running), `agentfs-node-daemon-xxx` (2/2 Running), and `cas-node-daemon-xxx` (2/2 Running).
*   `kube-objectfs-system`: `objectfs-controller-0` (2/2 Running), `objectfs-node-daemon-xxx` (2/2 Running), and `wal-buffer-0` (1/1 Running).

### 3. Validate Controller Connections
Confirm that controllers have initiated successfully and are listening:
```bash
kubectl logs -n kube-agentfs-system statefulset/agentfs-controller -c agentfs-controller | grep -i "listening"
kubectl logs -n kube-objectfs-system statefulset/objectfs-controller -c objectfs-controller | grep -i "listening"
```

---

## Teardown

To completely clean up and delete the deployed resources from Google Cloud, execute:

### 1. Delete Subsystem Workloads and Namespaces
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

### 2. Tear Down GKE Cluster
Delete the entire GKE cluster to completely stop compute billing:
```bash
gcloud container clusters delete "${CLUSTER_NAME}" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" --quiet
```

### 3. Delete Artifact Registry Repository
Delete the private container repository:
```bash
gcloud artifacts repositories delete "${GAR_REPOSITORY}" --location="${GAR_LOCATION}" --project="${GCP_PROJECT}" --quiet
```
