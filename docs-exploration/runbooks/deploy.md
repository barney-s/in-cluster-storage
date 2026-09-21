# Runbook: Deploying In-Cluster Storage Subsystems

*Changed since last revision: Integrated the repository's native `ap` CLI tool for streamlined image building, local Kind workflows, and subsystem deployment.*

## What this needs

**Tier**: 2 — CSI driver node-daemons require host mounts (`/var/lib/kubelet`), privileged DaemonSets, and kubelet plugin socket registration, which cannot run in a vcluster.

To execute this deployment, you will need:
- **Local Tooling**:
  - **Go SDK** (v1.27.1+) to invoke the `ap` tool.
  - **Docker** (or compatible container runtime) to build and containerize the images.
  - **kubectl** to manage Kubernetes resources.
  - **kind** (Kubernetes in Docker) for the preferred, least-privileged local development option.
- **Credentials & Permissions**:
  - Local `kind` cluster: Standard user-level shell credentials.
  - Remote cluster (alternative): `cluster-admin` RBAC permissions to create custom namespaces, `CSIDriver` definitions, custom `ClusterRole`/`ClusterRoleBindings`, and privileged `DaemonSets`.
- **Teardown Cost**: Zero financial cost when using local `kind`. Remote clusters (e.g., GKE) incur standard cloud instance rates until torn down.

## Preconditions

1. Ensure Docker is running locally:
   ```bash
   docker info
   ```
2. Confirm Go is installed and configured:
   ```bash
   go version
   ```

## Steps

### Path — Kind (tier 2)

This is the least-privileged and preferred development path using a local, ephemeral `kind` cluster.
*Alternative target (GKE / Remote Cluster): Point your `kubectl` context to your cluster, tag/push built images to your registry, and run `go run github.com/gke-labs/gke-labs-infra/ap@latest deploy`.*

#### 1. Create a Local Kind Cluster
```bash
kind create cluster --name local-storage-dev
```

#### 2. Build and Load Subsystem Images
Build the container images locally and load them directly into your Kind cluster.

**Using the Native `ap` Tool (Canonical):**
```bash
# Build all subsystem images
go run github.com/gke-labs/gke-labs-infra/ap@latest build

# Load the built images into Kind
kind load docker-image agentfs-controller:latest --name local-storage-dev
kind load docker-image agentfs-node-daemon:latest --name local-storage-dev
kind load docker-image objectfs-controller:latest --name local-storage-dev
kind load docker-image objectfs-node-daemon:latest --name local-storage-dev
kind load docker-image wal-buffer:latest --name local-storage-dev
kind load docker-image cas-node-daemon:latest --name local-storage-dev
```

**Using Standard Docker Build (Manual Fallback):**
```bash
docker build -t agentfs-controller:latest -f images/agentfs-controller/Dockerfile .
docker build -t agentfs-node-daemon:latest -f images/agentfs-node-daemon/Dockerfile .
docker build -t objectfs-controller:latest -f images/objectfs-controller/Dockerfile .
docker build -t objectfs-node-daemon:latest -f images/objectfs-node-daemon/Dockerfile .
docker build -t wal-buffer:latest -f images/wal-buffer/Dockerfile .
docker build -t cas-node-daemon:latest -f images/cas-node-daemon/Dockerfile .
```

#### 3. Deploy the Subsystems
Deploy the subsystems onto your Kind cluster.

**Using the Native `ap` Tool (Canonical):**
```bash
# Deploy all configurations; --skip-push prevents pushing to remote registries
go run github.com/gke-labs/gke-labs-infra/ap@latest deploy --skip-push
```

**Using kubectl (Manual Fallback):**
Configure local images to avoid pulling from external registries:
```bash
# Deploy AgentFS (kube-agentfs-system namespace)
kubectl create namespace kube-agentfs-system --dry-run=client -o yaml | kubectl apply -f -
sed -e 's|imagePullPolicy: Always|imagePullPolicy: Never|g' k8s/manifest.yaml | kubectl apply -f -

# Deploy ObjectFS & WAL Buffer (kube-objectfs-system namespace)
kubectl create namespace kube-objectfs-system --dry-run=client -o yaml | kubectl apply -f -
sed -e 's|imagePullPolicy: Always|imagePullPolicy: Never|g' k8s/wal.yaml | kubectl apply -f -
sed -e 's|imagePullPolicy: Always|imagePullPolicy: Never|g' k8s/objectfs.yaml | kubectl apply -f -
```

*(Optional) Deploy CAS CSI Driver:*
```bash
# Create and apply CAS CSI Driver and node-daemon configs derived from e2e tests
kubectl apply -f - <<EOF
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
          imagePullPolicy: Never
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

Confirm healthy initialization and running status of all deployed subsystems.

### 1. Inspect Pod and CSI Statuses
Ensure controllers and agents are Running and CSI drivers are registered:
```bash
# Verify CSI Drivers are registered
kubectl get csidrivers | grep -E 'agentfs|objectfs'

# Verify AgentFS & ObjectFS pods are Running
kubectl get pods -A -l 'app in (agentfs-controller, agentfs-node-daemon, objectfs-controller, objectfs-node-daemon, wal-buffer)'
```

### 2. Run Automated Verification Tests
Validate deployment correctness using the repository's native E2E suite:
```bash
go run github.com/gke-labs/gke-labs-infra/ap@latest e2e .
```

## Teardown

To clean up resources and restore host state:

### 1. Delete Subsystems and Namespaces
```bash
# Delete CAS
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
```bash
kind delete cluster --name local-storage-dev
```
