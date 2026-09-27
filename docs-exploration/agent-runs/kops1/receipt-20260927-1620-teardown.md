TORN-DOWN

# Teardown Receipt: kops1

This is the teardown receipt for the in-cluster-storage subsystems deployment **kops1** on KOPS on Google Compute Engine (GCE), executed on September 27, 2026.

All cloud and cluster resources owned by instance **kops1** have been successfully and completely destroyed.

---

## Verdict: TORN-DOWN

Every item listed in the deployment's "left RUNNING" contract has been verified as deleted. There are no residual resources or stragglers remaining in project `barni-cnrm-20260529`.

---

## Resources Removed & Verifying Evidence

### 1. Self-Managed KOPS Kubernetes Cluster (`ics-kops1.k8s.local`)
- **Action**: Ran `kops delete cluster --name=ics-kops1.k8s.local --state=gs://ics-kops1-kops-state --yes`.
- **Removed**:
  - 1x GCE Master Control-Plane VM (`e2-standard-2`)
  - 3x GCE Worker Node VMs (`e2-standard-2`)
  - Attached persistent boot, etcd-main, and etcd-events disks
  - Accompanying VPC networking, subnets, route tables, target pools, forwarding rules, and firewalls
- **Evidence**: `gcloud compute instances list` and `gcloud compute disks list` queries filtered by `ics-kops1` returned exactly `0 items`.

### 2. State Store GCS Bucket (`gs://ics-kops1-kops-state`)
- **Action**: Ran `gcloud storage buckets delete gs://ics-kops1-kops-state --quiet`.
- **Evidence**: `gcloud storage buckets list` confirms the bucket `gs://ics-kops1-kops-state` is no longer present in the project.

### 3. Google Artifact Registry (`ics-kops1-gar`)
- **Action**: Ran `gcloud artifacts repositories delete ics-kops1-gar --location=us-central1 --project=barni-cnrm-20260529 --quiet`.
- **Evidence**: `gcloud artifacts repositories list` confirms the repository is deleted.

---

## Verification Command Outputs

The following terminal queries confirm the successful destruction of all resources matching the resource prefix `ics-kops1`:

```
$ gcloud storage buckets list --project="barni-cnrm-20260529" --filter="name:ics-kops1"
Listed 0 items.

$ gcloud artifacts repositories list --project="barni-cnrm-20260529" --location="us-central1" --filter="name:ics-kops1"
Listed 0 items.

$ gcloud compute instances list --project="barni-cnrm-20260529" --filter="name:ics-kops1"
Listed 0 items.

$ gcloud compute disks list --project="barni-cnrm-20260529" --filter="name:ics-kops1"
Listed 0 items.

$ gcloud compute networks list --project="barni-cnrm-20260529" --filter="name:ics-kops1"
Listed 0 items.

$ gcloud compute firewall-rules list --project="barni-cnrm-20260529" --filter="name:ics-kops1"
Listed 0 items.
```

---

## Procedure Amended
No amendments were required for `docs-exploration/runbooks/deploy-kops-gce.md` or `docs-exploration/agent-runs/kops1/teardown.sh` as the automated teardown script executed flawlessly and performed all necessary tasks deterministically.
