# Overview

`in-cluster-storage` is a suite of container-native, high-performance in-cluster storage solutions designed for Kubernetes. It aims to solve the latency, secret isolation, memory overhead, and resource-amplification challenges associated with traditional storage mounts in cloud-native environments.

---

## The Core Subsystems

The repository is organized into four independent, highly optimized subsystems:

### 1. AgentFS (`agentfs`)
A snapshot-centric, versioned CSI filesystem driver utilizing **OverlayFS** and optional **EROFS**-optimized base-layers.
* **Problem Solved:** Traditional CSI mounts copy full datasets on start and scan the entire volume on stop to find modifications, causing high CPU/IO bottlenecks.
* **Solution:** Structuring volume mounts into read-only `lower/` (base snapshot) and writable `upper/` (OverlayFS). Unmounting scans *only* the `upper/` layer for delta-based snapshot pushes.
* **Hybrid Lazy-Loading:** Large datasets can start instantly by stubbing files on mount and lazy-loading file blocks on-demand using a kernel `fanotify` page interceptor.
* **EROFS Integration:** Compiles snapshot directory trees and metadata into a block-aligned, read-only EROFS image. Populates the node's base layer with exactly *one* `mount(2)` system call instead of $O(N)$ filesystem creation syscalls.

### 2. ObjectFS (`objectfs`)
A distributed, multi-writer FUSE CSI driver backed by object stores (S3/GCS).
* **Problem Solved:** Sharing object storage keys to every worker node exposes credentials, while downloading giant datasets introduces high node-level memory/disk overhead.
* **Solution:** Isolates GCS/S3 credentials to a central `objectfs-controller`. Node-level `objectfs-node-daemon` pods mount FUSE POSIX filesystems and communicate with the controller via gRPC.
* **Push Notifications:** The controller broadcasts invalidation events over streaming gRPC connections to ensure nodes synchronously invalidate and refresh their metadata caches when concurrent writes happen.

### 3. Content Addressable Storage (`cas`)
A high-throughput, content-addressable storage CSI driver (`cas.labs.gke.io`).
* **Problem Solved:** Reading locally cached blobs from container filesystems usually requires copying data through user-space network/Unix sockets, degrading CPU and latency.
* **Solution:** On mount, the daemon binds a Unix Domain Socket at `.in-cluster-storage/api` inside the volume target. Containers query blobs by SHA256.
* **Zero-Copy SCM_RIGHTS:** The CAS server passes an open file descriptor (FD) of the locally cached host file to the container using `SCM_RIGHTS`. The container directly reads/mmap-maps the file, achieving absolute zero-copy reads and maximum node page cache sharing.

### 4. WAL Buffer (`wal`)
A standalone, high-performance write-ahead log buffer service.
* **Problem Solved:** Distributed apps need to log records rapidly to a durable store without paying the full latencies of immediate cloud bucket commits.
* **Solution:** Provides a bidirectional gRPC logging stream (`Append`) that returns cumulative acks once flushed to local disk (`witness`) and asynchronously flushes segments to permanent storage (S3/GCS) via a `Flush` manifest sequence.
