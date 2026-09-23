# Repository Exploration Notes

This directory houses living, developer-oriented documentation outlining the core architecture, layout, and purpose of the `in-cluster-storage` repository.

## Index

*   [Session: Streams Work Next Steps (Sept 23, 2026)](sessions/2026-09-23-streams-work-next-steps.md) — Architectural session outlining the next engineering steps and design plans for WAL, ObjectFS, and CAS streams. *(Freshest)*
*   [Comparison: ObjectFS vs. JuiceFS](comparisons/juicefs.md) — Structural and architectural comparison of ObjectFS vs. JuiceFS metadata synchronization and security boundaries.
*   [Comparison: WAL Buffer vs. Kafka & BookKeeper](comparisons/wal-vs-kafka-bookkeeper.md) — Analysis of WAL Buffer record streaming vs. enterprise event streaming and ledger backends.
*   [KOPS GCE Deployment Runbook](runbooks/deploy-kops-gce.md) — Executable step-by-step instructions to build, deploy, verify, and teardown storage subsystems on KOPS-managed Kubernetes clusters on GCE.
*   [GKE Upgrade Runbook](runbooks/upgrade-gcp.md) — Executable step-by-step instructions to perform zero-downtime rolling upgrades of the in-cluster storage subsystems on GKE Standard.
*   [GKE Deployment Runbook](runbooks/deploy-gcp.md) — Executable step-by-step instructions to build, deploy, verify, and teardown storage subsystems on GKE Standard.
*   [Recent Activity (Sept 21, 2026)](activity/2026-09-21.md) — Summary of major themes, code churn, and key merges over the last month.
*   [Open Questions](questions.md) — Unresolved architectural ambiguities, scaling challenges, and future design paths.
*   [Code Map](code-map.md) — Layout of directories, 20 most critical source files, and "Danger Zone" files sensitive to edits.
*   [Architecture](architecture.md) — Structural blueprints and Mermaid data-flow sequence/component diagrams for each subsystem.
*   [Overview](overview.md) — High-level purpose, problem statements, and description of the four core storage solutions.
*   [Exploration Contract](SKILL.md) — Rules, structure, and constraints for maintaining these documentation files.
