# Code Map

This document outlines the codebase layout, highlights the key files, and identifies parts of the project that are sensitive to modifications.

---

## 1. Directory Structure

*   `cas/` — Self-contained CAS driver module (implements content-addressable storage CSI daemon & clients).
*   `cmd/` — Entry points / main packages for AgentFS, ObjectFS, and WAL components.
*   `pkg/` — Core packages and reusable libraries:
    *   `pkg/api/` — Generated gRPC protobuf bindings.
    *   `pkg/erofs/` — Go-native EROFS image writer and parser.
    *   `pkg/objectfs/` — FUSE filesystems, controller state machines, and volume models.
    *   `pkg/objectstore/` — Unified storage layer interfaces (GCS, S3, Memory, Local FS).
    *   `pkg/wal/` — Segmented write-ahead logs, server daemon, and client helpers.
*   `proto/` — Protobuf definitions defining gRPC APIs.
*   `tests/` — End-to-end, benchmark, and integration suites.

---

## 2. Key Files

The following files represent the core logic and entry points:

### Subsystems & Interfaces
1.  `proto/agentfs.proto` / `proto/objectfs.proto` / `proto/wal.proto` — Core gRPC API definitions.
2.  `pkg/objectstore/objectstore.go` — Unified storage backend abstraction.

### AgentFS (OverlayFS CSI)
3.  `cmd/agentfs-controller/main.go` — Serves snapshot metadata (`latest.pb`) and stores blobs.
4.  `cmd/agentfs-node-daemon/main.go` — Mounts CSI OverlayFS layers and handles delta snapshot uploads.

### EROFS Metadata Tooling
5.  `pkg/erofs/erofs.go` — Standard block-aligned EROFS compiler and read-only parser in pure Go.
6.  `pkg/erofs/erofs_test.go` — Detailed unit tests validating compiler-to-parser correctness.

### ObjectFS (FUSE multi-writer)
7.  `cmd/objectfs-controller/main.go` — Centralized multi-writer controller protecting object storage credentials.
8.  `pkg/objectfs/controller/service.go` — Main gRPC service, metadata tree, and push broadcaster.
9.  `pkg/objectfs/controller/volume.go` — Directory, file inode representation, and chunk management.
10. `cmd/objectfs-node-daemon/main.go` — CSI Node node mounting ObjectFS FUSE volumes.
11. `pkg/objectfs/fuse/fs.go` — POSIX filesystem layer mapping FUSE calls into ObjectFS gRPC commands.
12. `pkg/objectfs/fuse/cache.go` — Metadata lookup and attribute cache on node.

### Content Addressable Storage (CAS)
13. `cas/cmd/cas-node-daemon/main.go` — CSI driver spawning local UDS CAS servers on Mount.
14. `cas/pkg/cas/server.go` — Accepts connections and transfers file descriptors using `SCM_RIGHTS`.
15. `cas/pkg/cas/client.go` — Client wrapper extracting raw FDs from Out-Of-Band (OOB) control messages.

### WAL Buffer
16. `cmd/wal-buffer/main.go` — Server running the fast Write-Ahead Log Buffer service.
17. `pkg/wal/buffer/server.go` — Stream-multiplexer matching streams, allocating positions, and caching segments.
18. `pkg/wal/record.go` — Layout mapping, serializing, and crc32c auditing of WAL entries.
19. `pkg/wal/segment.go` — Handles thread-safe local file-segment operations on node.
20. `pkg/wal/client/client.go` — Thread-safe WAL streaming client for consumers.

---

## 3. The Danger Zone (Modify with Care)

*   `pkg/erofs/erofs.go` — EROFS metadata is strictly block-aligned (typically 4096 bytes). Changes to struct alignment, padding, or directory entry layouts will make generated images un-mountable by Linux kernels.
*   `cas/pkg/cas/server.go` — Relies on Unix domain sockets and low-level system calls (`unix.WriteMsgUnix`). Small edits in OOB control buffer assembly or close sequences can trigger FD leaks, connection deadlocks, or `EBUSY` unmount errors.
*   `pkg/objectfs/fuse/fs.go` — Connects kernel FUSE system calls to Go. Unsynchronized state changes, improper lock delegation, or missing lock unlocks can deadlock container runtimes or cause zombie mount processes.
*   `pkg/wal/record.go` — Manages binary WAL record structures, serialization, and CRC32C audits. Changing the record structure or serialization logic will break backwards compatibility with on-disk segments and cause recovery failures on restart.
