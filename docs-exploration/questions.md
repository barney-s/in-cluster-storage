# Open Questions

This document tracks unresolved architectural ambiguities, scaling limits, and potential future explorations.

---

## 1. Subsystem Integration & Consolidation
* **Unified Daemon vs. Separate Daemons:** Currently, AgentFS (`agentfs.labs.gke.io`), ObjectFS (`objectfs.labs.gke.io`), and CAS (`cas.labs.gke.io`) deploy as three separate CSI drivers and three distinct DaemonSets.
  * *Question:* Are there future plans to unify them into a single, modular multi-driver DaemonSet image to reduce host resource footprint and Kubernetes API overhead on larger clusters?

---

## 2. Content Addressable Storage (CAS) Lifecycle
* **Local Node Cache Eviction:** The CAS daemon caches pulled blobs under a local storage directory (`/var/cache/cas` or custom `--storage-path`).
  * *Question:* How is local host disk space managed/evicted when the node is low on disk? Is there an active Least-Recently-Used (LRU) background eviction routine inside `cas/pkg/cas/server.go`, or does the driver rely on external orchestration/cron for cleanup?

---

## 3. fanotify Scaling & Portability
* **User-Space Event Limits:** For large volumes containing tens of thousands of files, hybrid lazy-loading relies on monitoring file open events (`FAN_OPEN_PERM`) via `fanotify`.
  * *Question:* At what scale (number of active pods or open file handles) does the single-threaded context-switching overhead of user-space `fanotify` interception become a bottleneck?
  * *Question:* Are there specific Linux kernel minimum requirements or kernel config flags (other than standard `fanotify` support) needed to guarantee compatibility across diverse cloud-provider host kernels (e.g., Bottlerocket, COS, Ubuntu)?

---

## 4. ObjectFS Multi-Writer Consistency
* **Strict POSIX and Write Consistency:** ObjectFS embraces eventual write consistency through a 3-tier local buffering design and periodic background backend flushing.
  * *Question:* For distributed workloads or databases that require read-after-write or strong lock-based consistency across nodes, are there plans to introduce synchronous flush overrides, transactional writes, or standard POSIX file locking (`flock`), or is ObjectFS positioned solely for loose-consistency / read-heavy use cases?

---

## 5. Standardized CAS Deployment Manifests
* **Lack of Standalone Manifest File:** Currently, CAS is deployed during E2E tests using inline manifests in `tests/e2e/cas_e2e_test.go`, but unlike AgentFS (`k8s/manifest.yaml`) and ObjectFS (`k8s/objectfs.yaml`), it does not have a dedicated `k8s/cas.yaml` file.
  * *Question:* Should we export and maintain a standard standalone CAS deployment manifest inside the `k8s/` folder for consistency and ease of manual deployment?

---

## 6. Local Development ImagePullPolicy with `ap deploy`
* **Image Pull Failure on Local Clusters:** The deployment manifests in `k8s/` use the `:latest` image tag without specifying an `imagePullPolicy`. When loaded into local `kind` clusters, Kubernetes defaults to `imagePullPolicy: Always` for `:latest` tags, causing `ImagePullBackOff` as it attempts to pull from a remote registry.
  * *Question:* Can `ap deploy` be configured to dynamically inject/override `imagePullPolicy: Never` or `imagePullPolicy: IfNotPresent` when targeting local clusters with `--skip-push`, or should developers continue to manually patch/rewrite the manifests before deployment?

---

## 7. Native Container Registry Overrides in `ap` CLI
* **Lack of Configurable Target Registry:** Currently, the `ap build` and `ap deploy` commands do not expose CLI parameters or config-file options to specify a remote container registry (e.g., Google Artifact Registry under `cnrm-barni-2`).
  * *Question:* How should multi-environment deployment registries be defined natively inside the `ap` tool (for example, through `ap config` or environment-variable bindings) to avoid manual tagging/pushing and manual manifest edits (`sed` scripting)?

---

## 8. IAM Integration & Workload Identity for CSI Drivers on GKE
* **GCP Privileged Access on Remote Clusters:** The storage subsystems (like ObjectFS and WAL Buffer) interact with Google Cloud Storage (GCS) and AWS S3-compatible endpoints. On GKE, the canonical and secure way to grant these privileges is via Workload Identity Federation for GKE.
  * *Question:* Should the Kubernetes `ServiceAccount` templates under `k8s/` be pre-configured with annotations for Workload Identity (e.g., `iam.gke.io/gcp-service-account`), or is it assumed that users will attach IAM roles to the GKE worker node service accounts directly?

---

## 9. Secure Container Registry Access for KOPS-managed GCE Nodes
* **Private Artifact Registry Access under KOPS:** On standard GKE, node default scopes/IAM service accounts are pre-configured to easily read from Google Artifact Registry (GAR) within the same project. Under KOPS on GCE, nodes may not have access by default, forcing users to manually grant the `roles/artifactregistry.reader` role or configure `imagePullSecrets`.
  * *Question:* What is the recommended secure mechanism to configure KOPS clusters on GCE for private Artifact Registry integration? Should we configure GCP cloud scopes directly inside the KOPS `InstanceGroup` resources, or should we recommend using the GCR/GAR credential helper or Kubernetes `imagePullSecrets`?

---

## 10. KOPS GCE IAM Role Binding and setIamPolicy Permission Boundaries
* **Lack of setIamPolicy in Editor Role:** To deploy a KOPS cluster on GCE, the KOPS CLI expects to automatically configure IAM roles and service accounts, requiring `resourcemanager.projects.setIamPolicy`. In standard sandbox environments, deployers are often bound to `roles/editor`, which does not include this high-privilege permission.
  * *Question:* Should we document a minimal manual GCE IAM setup for KOPS so that users with `roles/editor` can still deploy KOPS on GCE without requiring project-level owner access? Or should we recommend that KOPS on GCE be deployed exclusively by identities holding `roles/owner` or custom roles with `setIamPolicy` capabilities?

---

## 12. Tracking and Version-Controlling kOps Cluster & Instance Group Manifests
* **Checking In kOps Configuration Files:** To comply with GitOps practices, the generated kOps cluster and instance group manifests are exported as YAML configurations and placed in the deployment-specific directory (e.g. `docs-exploration/runbook-deployments/ics3/manifests/`).
  * *Question:* Should we establish a standard schema for directory-based deployment tracking of these manifests across other staging and production environments? How should we automate updates back to these files when live cluster specs are changed via `kops edit cluster`?

---

## 11. Skip Justifications for In-Pod Deployment & Upgrade Runbooks
* **Lack of In-Pod Story for Privileged Systems:** The storage subsystems (`agentfs`, `objectfs`, `cas`, and `wal-buffer`) are built as low-level Kubernetes CSI drivers and write-ahead log daemons.
  * *Justification for skipping `deploy-in-pod.md` & `upgrade-in-pod.md`:* CSI driver node-daemons require actual Kubernetes nodes with host mount access (`/var/lib/kubelet`), bidirectional mount propagation, privileged containers, and kernel capabilities (`SYS_ADMIN` and `fanotify`). Since a local "in-pod" sandbox cannot provide actual node-level host mounts or privileged namespaces, it is impossible to deploy, upgrade, or run these subsystems directly inside a plain pod sandbox or local non-privileged environment. Unit and integration tests (under `tests/e2e/`) run local `kind` clusters instead of raw "in-pod" environments, which also require an external container runtime daemon (like Docker). Therefore, the `in-pod` environment does not genuinely apply to this repository's subsystems, and these runbooks have been skipped in accordance with the Exploration Contract.



