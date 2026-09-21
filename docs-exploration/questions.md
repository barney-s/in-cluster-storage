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
