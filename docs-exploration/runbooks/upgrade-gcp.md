# GKE Upgrade Runbook

*(Pinned)* **This runbook is executable top-to-bottom on GKE.** It details the step-by-step procedure to perform zero-downtime rolling upgrades of the in-cluster storage subsystems (`agentfs`, `objectfs`, `wal-buffer`, and `cas`) on Google Kubernetes Engine (GKE).

---

## What this needs

### Deployment Type & Upgrade Infrastructure Requirement
This upgrade runbook **requires an active Google Kubernetes Engine (GKE) Standard cluster** with the storage subsystems already deployed. It cannot run in-pod.
*   **Reason**: CSI drivers (`agentfs.labs.gke.io`, `objectfs.labs.gke.io`, `cas.labs.gke.io`) run as low-level DaemonSets and StatefulSets that integrate with GKE physical worker nodes (via direct host mounts `/var/lib/kubelet` and kernel capability `SYS_ADMIN`). Performing a rolling upgrade involves observing Kubernetes controller transitions and daemon pod replacement behaviors under active workload traffic, which requires a real Kubernetes environment.

### Enumed IAM Permissions
To execute this runbook, the calling Google Cloud identity must have the following IAM permissions on the target GCP project:
1.  **GKE Cluster Permissions**:
    *   `container.clusters.get` & `container.clusters.list`
    *   `container.clusters.update` (to update node pools or settings if needed during upgrade)
2.  **Artifact Registry Permissions**:
    *   `artifactregistry.repositories.get`
    *   `artifactregistry.repositories.uploadArtifacts` (to upload updated image versions)
3.  **Cloud Build Permissions**:
    *   `cloudbuild.builds.create` (to build updated container images)
4.  **Kubernetes RBAC Permissions**:
    *   Full administrative permissions inside namespaces `kube-agentfs-system` and `kube-objectfs-system` to apply updated manifests and query rollout statuses.

### Verified Feasibility Checklist
*   ✓ **`gcloud` CLI**: Present (Google Cloud SDK installed).
*   ✓ **`kubectl` CLI**: Present.
*   ✓ **`docker` CLI**: Present.
*   ✓ **Active GCP Credential**: Granted `roles/owner` or `roles/editor` on the active project `barni-cnrm-20260529`.

---

## Preconditions

Ensure that the storage subsystems are currently deployed (see [GKE Deployment Runbook](deploy-gcp.md)) and that you have exported the following target environment variables (specifying the upgrade target tag):

```bash
export GCP_PROJECT="barni-cnrm-20260529"
export GCP_REGION="us-central1"
export GCP_ZONE="us-central1-a"

export CLUSTER_NAME="ics-gke-cluster"
export GAR_LOCATION="${GCP_REGION}"
export GAR_REPOSITORY="in-cluster-storage"

# Current deployed version
export CURRENT_TAG="latest"

# Target upgraded version
export NEW_TAG="v2.0.0-upgrade"

export REGISTRY="${GAR_LOCATION}-docker.pkg.dev/${GCP_PROJECT}/${GAR_REPOSITORY}"
```

Connect your `kubectl` client to the GKE cluster:
```bash
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --zone="${GCP_ZONE}" \
  --project="${GCP_PROJECT}"
```

---

## Steps

### 1. Build and Push Target Upgrade Container Images
Compile and push the newer, upgraded versions of the 6 storage subsystem images to Google Artifact Registry:
```bash
PIDS=()
for img in agentfs-controller agentfs-node-daemon objectfs-controller objectfs-node-daemon wal-buffer cas-node-daemon; do
  cat <<EOF > "cloudbuild-${img}.yaml"
steps:
- name: 'gcr.io/cloud-builders/docker'
  args: [ 'build', '-f', 'images/${img}/Dockerfile', '-t', '${REGISTRY}/${img}:${NEW_TAG}', '.' ]
images:
- '${REGISTRY}/${img}:${NEW_TAG}'
EOF

  gcloud builds submit \
    --project="${GCP_PROJECT}" \
    --config="cloudbuild-${img}.yaml" \
    . > "build-${img}.log" 2>&1 &
  PIDS+=($!)
done

# Wait for parallel compilation and push to complete
for pid in "${PIDS[@]}"; do
  wait "${pid}"
done

# Clean up
rm -f build-*.log cloudbuild-*.yaml
```

### 2. Spawn an Active Workload Pod (Pre-Upgrade Traffic)
To guarantee zero-downtime and verify that active CSI mounts are **not interrupted** during the rolling upgrade, deploy a persistent test workload pod that writes to an `agentfs` mount:
```bash
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: upgrade-active-workload
  namespace: default
spec:
  restartPolicy: Never
  containers:
    - name: writer-app
      image: alpine
      command: ["/bin/sh", "-c", "while true; do echo \$(date) >> /data/heartbeat.txt; sync; sleep 2; done"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      csi:
        driver: agentfs.labs.gke.io
        volumeAttributes:
          volumeID: upgrade-volume-456
EOF

# Ensure the workload pod starts running successfully
kubectl wait --for=condition=Ready pod/upgrade-active-workload --timeout=1m
```

### 3. Apply Upgraded Manifests
Create upgraded configurations by replacing default `:latest` references with the new target `NEW_TAG` paths and apply them. Kubernetes will perform rolling updates:
```bash
mkdir -p build/upgraded-manifests

sed -e "s|image: agentfs-controller:latest|image: ${REGISTRY}/agentfs-controller:${NEW_TAG}|g" \
    -e "s|image: agentfs-node-daemon:latest|image: ${REGISTRY}/agentfs-node-daemon:${NEW_TAG}|g" \
    k8s/manifest.yaml > build/upgraded-manifests/manifest.yaml

sed -e "s|image: objectfs-controller:latest|image: ${REGISTRY}/objectfs-controller:${NEW_TAG}|g" \
    -e "s|image: objectfs-node-daemon:latest|image: ${REGISTRY}/objectfs-node-daemon:${NEW_TAG}|g" \
    k8s/objectfs.yaml > build/upgraded-manifests/objectfs.yaml

sed -e "s|image: wal-buffer:latest|image: ${REGISTRY}/wal-buffer:${NEW_TAG}|g" \
    k8s/wal.yaml > build/upgraded-manifests/wal.yaml

# Apply the newer specs
kubectl apply -f build/upgraded-manifests/manifest.yaml
kubectl apply -f build/upgraded-manifests/wal.yaml
kubectl apply -f build/upgraded-manifests/objectfs.yaml

# Apply upgraded CAS DaemonSet
sed -e "s|image: cas-node-daemon:latest|image: ${REGISTRY}/cas-node-daemon:${NEW_TAG}|g" <<'EOF' | kubectl apply -f -
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

### 4. Monitor and Verify Subsystem Rollouts
Observe Kubernetes transition the workloads step-by-step. The controllers roll out as StatefulSets, and the node drivers roll out as node-by-node DaemonSet rolling updates:
```bash
# Verify Controller rollouts (Wait up to 5 minutes)
kubectl rollout status statefulset/agentfs-controller -n kube-agentfs-system --timeout=5m
kubectl rollout status statefulset/objectfs-controller -n kube-objectfs-system --timeout=5m

# Verify DaemonSet rolling updates
kubectl rollout status daemonset/agentfs-node-daemon -n kube-agentfs-system --timeout=5m
kubectl rollout status daemonset/objectfs-node-daemon -n kube-objectfs-system --timeout=5m
kubectl rollout status daemonset/cas-node-daemon -n kube-agentfs-system --timeout=5m
kubectl rollout status statefulset/wal-buffer -n kube-objectfs-system --timeout=5m
```

---

## Verify

Execute the following post-upgrade validations to ensure complete behavioral and structural integrity:

### 1. Check Upgraded Workload Pod Versions
Ensure that all running containers are using the new target image tag (`v2.0.0-upgrade`):
```bash
kubectl get pods -n kube-agentfs-system -o jsonpath="{.items[*].spec.containers[*].image}" | tr ' ' '\n' | sort -u
kubectl get pods -n kube-objectfs-system -o jsonpath="{.items[*].spec.containers[*].image}" | tr ' ' '\n' | sort -u
```
*Expected Output*: Contains images pointing to `${REGISTRY}/...:v2.0.0-upgrade`.

### 2. Verify Zero-Downtime Active Workload Pod Stability
Inspect the `upgrade-active-workload` pod. It must remain in `Running` state and continue writing heartbeats through the entire CSI daemon replacement cycle without crashing or losing storage access:
```bash
# Check pod status (MUST be Running with 0 restarts)
kubectl get pod upgrade-active-workload -o wide

# Check written heartbeats to verify continuous I/O during the upgrade
kubectl exec upgrade-active-workload -- tail -n 5 /data/heartbeat.txt
```

### 3. Verify Local CAS Caches Persistence
Confirm that the `cas-node-daemon` successfully mounted and reused the pre-existing host path caches (`/var/cache/cas`) instead of wiping files. Check the startup logs of any newly deployed CAS daemon pod:
```bash
CAS_POD=$(kubectl get pods -n kube-agentfs-system -l app=cas-node-daemon -o jsonpath="{.items[0].metadata.name}")
kubectl logs -n kube-agentfs-system "${CAS_POD}" -c cas-node-daemon | grep -i -E "cache|storage|initialized"
```
*Expected Output*: Logs indicating detection/re-initialization of the existing storage path `/var/cache/cas` without data wipe warnings.

---

## Teardown

To clean up resources created during the upgrade verification:

### 1. Delete Active Verification Workload Pod
```bash
kubectl delete pod upgrade-active-workload --ignore-not-found
rm -rf build/upgraded-manifests
```

### 2. Standard Infrastructure Teardown
If you wish to tear down the entire cluster and registries after completing tests, refer to the **Teardown** section of the [GKE Deployment Runbook](deploy-gcp.md).
