# Runbook: Deploying In-Cluster Storage Subsystems

This runbook guides you through building and deploying the `in-cluster-storage` components onto a Kubernetes cluster.

## What this needs

To execute this deployment, you will need:
- **Local Tooling**:
  - **Go SDK** (version 1.27.1+ or as specified in `go.mod`) for building binaries and invoking `ap`.
  - **Docker** (or compatible container runtime) to build and containerize the images.
  - **kubectl** to manage Kubernetes resources.
  - **kind** (Kubernetes in Docker) for the least-privileged local development deployment (preferred/local option).
- **Target Environment**:
  - A Kubernetes cluster. You can use a local `kind` cluster (least-privileged option) or an existing remote cluster (e.g., GKE).
- **Credentials & Permissions**:
  - Standard user-level credentials to create a local `kind` cluster.
  - If deploying to a remote cluster, you must have `cluster-admin` RBAC permissions to create `CSIDriver` definitions, custom `ClusterRole`, `ClusterRoleBinding`, namespaces, and privileged `DaemonSets` / `StatefulSets`.
  - Node-level privileges: Worker nodes must support privileged containers and direct mounting of host volumes under `/var/lib/kubelet` (required for CSI drivers).
- **Estimated Cost of Teardown**: Zero financial cost when using local `kind`. On GKE/public cloud, it will cost standard VM instance/persistent storage rates until torn down.

## Preconditions

1. Ensure Docker is running locally:
   ```bash
   docker info
   ```
2. Confirm Go is installed:
   ```bash
   go version
   ```
3. A Kubernetes cluster is running and your current context points to it.
   - For a local `kind` cluster, create it via:
     ```bash
     kind create cluster --name local-storage-dev
     ```
   - For an existing cluster, verify connectivity:
     ```bash
     kubectl cluster-info
     ```

## Steps

### 1. Build Component Container Images
Build the container images for the subsystems you wish to deploy. We define tags below (e.g., `latest` or `local`), which must match the image references in the manifests.

```bash
# Build AgentFS images
docker build -t agentfs-controller:local -f images/agentfs-controller/Dockerfile .
docker build -t agentfs-node-daemon:local -f images/agentfs-node-daemon/Dockerfile .

# Build ObjectFS and WAL Buffer images
docker build -t objectfs-controller:local -f images/objectfs-controller/Dockerfile .
docker build -t objectfs-node-daemon:local -f images/objectfs-node-daemon/Dockerfile .
docker build -t wal-buffer:local -f images/wal-buffer/Dockerfile .

# Build CAS image (Optional)
docker build -t cas-node-daemon:local -f images/cas-node-daemon/Dockerfile .
```

### 2. Make Images Available to Cluster
If using a local **Kind** cluster, load the locally built images directly into Kind to avoid pushing to a registry:

```bash
# Load AgentFS
kind load docker-image agentfs-controller:local --name local-storage-dev
kind load docker-image agentfs-node-daemon:local --name local-storage-dev

# Load ObjectFS and WAL Buffer
kind load docker-image objectfs-controller:local --name local-storage-dev
kind load docker-image objectfs-node-daemon:local --name local-storage-dev
kind load docker-image wal-buffer:local --name local-storage-dev

# Load CAS
kind load docker-image cas-node-daemon:local --name local-storage-dev
```

*Note: For remote/production environments, tag and push these images to a container registry (e.g., Google Artifact Registry, `gcr.io` or `pkg.dev`) and update the manifest files with the correct image paths.*

### 3. Deploy the Subsystems

#### Option A: Deploy AgentFS
AgentFS is deployed using the manifest file `k8s/manifest.yaml`. 

First, prepare the manifest by replacing the image tags or namespaces if desired. By default, it deploys to the `kube-agentfs-system` namespace. If deploying to `kube-agentfs-system`, ensure the namespace exists:

```bash
kubectl create namespace kube-agentfs-system --dry-run=client -o yaml | kubectl apply -f -
```

Apply the manifest with the local image overrides (or customize `k8s/manifest.yaml` in-place):

```bash
# We can apply the manifest file directly
# (If using local images, ensure k8s/manifest.yaml points to 'local' tags and imagePullPolicy is set to 'Never')
sed -e 's|agentfs-controller:latest|agentfs-controller:local|g' \
    -e 's|agentfs-node-daemon:latest|agentfs-node-daemon:local|g' \
    -e 's|imagePullPolicy: Always|imagePullPolicy: Never|g' \
    k8s/manifest.yaml | kubectl apply -f -
```

#### Option B: Deploy ObjectFS & WAL Buffer
ObjectFS and WAL Buffer deploy to the `kube-objectfs-system` namespace. Create the namespace:

```bash
kubectl create namespace kube-objectfs-system --dry-run=client -o yaml | kubectl apply -f -
```

Deploy the WAL Buffer StatefulSet and Service:

```bash
sed -e 's|wal-buffer:latest|wal-buffer:local|g' \
    -e 's|imagePullPolicy: Always|imagePullPolicy: Never|g' \
    k8s/wal.yaml | kubectl apply -f -
```

Deploy the ObjectFS CSI Driver, Controller, and Node Daemon:

```bash
sed -e 's|objectfs-controller:latest|objectfs-controller:local|g' \
    -e 's|objectfs-node-daemon:latest|objectfs-node-daemon:local|g' \
    -e 's|imagePullPolicy: Always|imagePullPolicy: Never|g' \
    k8s/objectfs.yaml | kubectl apply -f -
```

#### Option C: Deploy Content Addressable Storage (CAS) CSI Driver (Optional)
The CAS subsystem is not shipped with a standalone manifest in `k8s/`, but we can construct its CSI driver and DaemonSet configuration derived from `tests/e2e/cas_e2e_test.go`:

```bash
# Save and apply the CAS CSI Driver configuration
cat <<EOF | kubectl apply -f -
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: cas.labs.gke.io
spec:
  attachRequired: false
  podInfoOnMount: true
  volumeLifecycleModes:
    - Ephemeral
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: cas-node-daemon
  namespace: kube-agentfs-system # Shared namespace or default
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
          args:
            - "--v=5"
            - "--csi-address=\$(ADDRESS)"
            - "--kubelet-registration-path=\$(DRIVER_REG_SOCK_PATH)"
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
          image: cas-node-daemon:local
          imagePullPolicy: Never
          args:
            - "--v=5"
            - "--endpoint=unix:///csi/csi.sock"
            - "--nodeid=\$(NODE_ID)"
            - "--storage-path=/var/cache/cas"
            - "--controller-address=agentfs-controller.kube-agentfs-system.svc.cluster.local:50051"
          env:
            - name: NODE_ID
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
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
          hostPath:
            path: /var/lib/kubelet/plugins/cas.labs.gke.io/
            type: DirectoryOrCreate
        - name: registration-dir
          hostPath:
            path: /var/lib/kubelet/plugins_registry/
            type: Directory
        - name: kubelet-dir
          hostPath:
            path: /var/lib/kubelet
            type: Directory
        - name: storage-dir
          hostPath:
            path: /var/cache/cas
            type: DirectoryOrCreate
EOF
```

## Verify

Verify the operational status of the deployed components and confirm healthy startup.

### 1. Check AgentFS Driver Health
```bash
# Verify CSI Driver Registration
kubectl get csidrivers | grep agentfs.labs.gke.io

# Verify Controller and Node Daemon pods are Running
kubectl get pods -n kube-agentfs-system -l app=agentfs-controller
kubectl get pods -n kube-agentfs-system -l app=agentfs-node-daemon

# View startup logs to verify healthy grpc server init
kubectl logs -n kube-agentfs-system statefulset/agentfs-controller -c agentfs-controller
kubectl logs -n kube-agentfs-system daemonset/agentfs-node-daemon -c agentfs-node-daemon
```

### 2. Check ObjectFS & WAL Buffer Health
```bash
# Verify CSI Driver Registration
kubectl get csidrivers | grep objectfs.labs.gke.io

# Verify WAL Buffer, ObjectFS Controller and Node Daemon pods
kubectl get pods -n kube-objectfs-system -l app=wal-buffer
kubectl get pods -n kube-objectfs-system -l app=objectfs-controller
kubectl get pods -n kube-objectfs-system -l app=objectfs-node-daemon

# Verify StorageClass creation
kubectl get storageclass objectfs-standard
```

### 3. Verify via End-to-End E2E Runner (Repository Tooling)
The repository includes automated validation via the CI pipeline scripts. You can run these local validation workflows to execute a full deployment, run pods writing/reading data, and verify functionality in a throwaway kind cluster:

```bash
# Run ap-e2e (Requires a local kind cluster, handles image building, loading, deployment, and testing automatically)
export RUN_E2E=true
./dev/ci/presubmits/ap-e2e
```

## Teardown

To clean up resources and restore your system state, execute the following.

### 1. Delete Subsystems via manifests
```bash
# Delete CAS CSI driver
kubectl delete csidriver cas.labs.gke.io --ignore-not-found
kubectl delete daemonset cas-node-daemon -n kube-agentfs-system --ignore-not-found

# Delete ObjectFS and WAL Buffer
kubectl delete -f k8s/objectfs.yaml --ignore-not-found
kubectl delete -f k8s/wal.yaml --ignore-not-found
kubectl delete namespace kube-objectfs-system --ignore-not-found

# Delete AgentFS
kubectl delete -f k8s/manifest.yaml --ignore-not-found
kubectl delete namespace kube-agentfs-system --ignore-not-found
```

### 2. Destroy Local Kind Cluster
If you set up a local throwaway Kind cluster for deployment, destroy it to free system resources:
```bash
kind delete cluster --name local-storage-dev
```
