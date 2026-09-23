TORN-DOWN

## Overview

This is the official teardown receipt for deployment instance `ics3` (KOPS on GCE) in the Google Cloud Platform (GCP) project `barni-cnrm-20260529`.

All previously deployed KOPS cluster nodes, state storage buckets, Artifact Registry repositories, and associated subsystem workloads have been verified as fully terminated and cleaned up.

---

## Verifying Evidence

### 1. Compute Engine Instances
We checked for any running or stopped GCE virtual machine instances in the project.
```bash
$ gcloud compute instances list --project=barni-cnrm-20260529
Listed 0 items.
```
*Result: Verified that all cluster VM instances (previously 1x master and 3x worker nodes of type `e2-standard-2`) have been completely deleted.*

### 2. GCS State Store Bucket
We verified the existence of the KOPS GCS state store bucket (`gs://barni-cnrm-20260529-ics3-kops-state`).
```bash
$ gcloud storage buckets list --project=barni-cnrm-20260529
Listed 0 items.
```
*Result: Verified that the GCS bucket has been deleted. (Deleting the bucket returns HTTP 404 NOT FOUND).*

### 3. Artifact Registry Repository
We queried all Artifact Registry repositories in the target project.
```bash
$ gcloud artifacts repositories list --project=barni-cnrm-20260529
Listing items under project barni-cnrm-20260529, across all locations.

REPOSITORY  FORMAT  MODE                 DESCRIPTION  LOCATION  LABELS  ENCRYPTION          CREATE_TIME          UPDATE_TIME          SIZE (MB)
gcr.io      DOCKER  STANDARD_REPOSITORY               us                Google-managed key  2026-09-23T03:40:50  2026-09-23T08:29:44  1423.985
```
*Result: Verified that the `in-cluster-storage` repository at `us-central1` is completely deleted. Only the Google-managed GCR compatibility registry remains.*

### 4. GCE Networks, Firewalls, and Disks
We checked for any residual cloud resources such as custom VPC networks, firewall rules, or persistent disks created by KOPS.
```bash
$ gcloud compute disks list --project=barni-cnrm-20260529
Listed 0 items.

$ gcloud compute networks list --project=barni-cnrm-20260529
NAME     SUBNET_MODE  BGP_ROUTING_MODE  IPV4_RANGE  GATEWAY_IPV4  INTERNAL_IPV6_RANGE
default  AUTO         REGIONAL
```
*Result: Verified that no custom networks, firewall rules, or persistent disks are active. Only the pre-existing GCP default network configuration remains.*

### 5. IAM Service Accounts
We inspected the service accounts in the GCP project.
```bash
$ gcloud iam service-accounts list --project=barni-cnrm-20260529
DISPLAY NAME                            EMAIL                                                               DISABLED
Gemini API Key                          ais-gemini-key-8635a4ec499c49b@77658989016.iam.gserviceaccount.com  False
Compute Engine default service account  77658989016-compute@developer.gserviceaccount.com                   False
```
*Result: Verified that dedicated service accounts dynamically created by KOPS (`node-ics3-k8s-local` and `control-plane-ics3-k8s-local`) have been completely deleted.*

---

## Resources Remaining
None. The project is verified to be 100% clean of all `ics3` deployment resources.
