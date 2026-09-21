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

