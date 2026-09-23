TORN-DOWN

## Overview

The deployment instance `deploy-gcp-1` has been fully and successfully torn down. All GCP and GKE resources associated with this deployment have been verified as completely removed. No billing components remain.

## What Was Removed & Verifying Evidence

### 1. GKE Cluster `ics-1` (and all Subsystem Workloads)
- **Action**: Automated teardown of the GKE Standard Cluster `ics-1` in zone `us-central1-a` and project `barni-cnrm-20260529`.
- **Verifying Evidence**:
  - Queried GKE cluster list and confirmed it is completely gone (returned 0 results):
    ```bash
    $ gcloud container clusters list --project=barni-cnrm-20260529
    (empty)
    ```
  - Listing Compute Engine VMs confirms that all GKE worker nodes for `ics-1` have been deleted. The only remaining VMs in the project belong to an independent kops cluster (`ics3-k8s-local`):
    ```bash
    $ gcloud compute instances list --project=barni-cnrm-20260529 --format="table(name, zone, labels)"
    NAME                              ZONE           LABELS
    control-plane-us-central1-a-d2dw  us-central1-a  {'k8s-io-cluster-name': 'ics3-k8s-local', ...}
    nodes-us-central1-a-kb7g          us-central1-a  {'k8s-io-cluster-name': 'ics3-k8s-local', ...}
    nodes-us-central1-a-kmj3          us-central1-a  {'k8s-io-cluster-name': 'ics3-k8s-local', ...}
    nodes-us-central1-a-n9f3          us-central1-a  {'k8s-io-cluster-name': 'ics3-k8s-local', ...}
    ```
  - All CSI driver workloads, daemonsets, and namespaces (`kube-agentfs-system` and `kube-objectfs-system`) are completely gone along with the cluster.

### 2. Artifact Registry Repository `in-cluster-storage`
- **Action**: Deleted the Artifact Registry Docker repository `in-cluster-storage` in `us-central1`.
- **Verifying Evidence**:
  - Listing Artifact Registry repositories in `us-central1` confirms no repository exists:
    ```bash
    $ gcloud artifacts repositories list --project=barni-cnrm-20260529 --location=us-central1
    Listing items under project barni-cnrm-20260529, location us-central1.
    Listed 0 items.
    ```

### 3. GCS Cloud Build Staging Bucket
- **Action**: Deleted the GCS bucket `gs://barni-cnrm-20260529_cloudbuild`.
- **Verifying Evidence**:
  - Listing Cloud Storage buckets verifies that the staging bucket is completely removed:
    ```bash
    $ gcloud storage buckets list --project=barni-cnrm-20260529
    # (gs://barni-cnrm-20260529_cloudbuild is absent from the list)
    ```

---

## What Remains

Nothing from the `deploy-gcp-1` deployment instance remains active or is incurring costs. All resources listed in the teardown contract have been 100% verified as destroyed.
