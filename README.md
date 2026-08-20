# TPU Resource Driver for Dynamic Resource Allocation (DRA)

This repository contains a TPU resource driver for use with the [Dynamic
Resource Allocation
(DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
feature of Kubernetes.

## Quickstart and Demo

This demo walks through the process of building and installing the driver followed by running a set of workloads that consume TPUs.

### Prerequisites

* [GNU Make 3.81+](https://www.gnu.org/software/make/)
* [GNU Tar 1.34+](https://www.gnu.org/software/tar/)
* [docker v20.10+ (including buildx)](https://docs.docker.com/engine/install/) or [Podman v4.9+](https://podman.io/docs/installation)
* [helm v3.7.0+](https://helm.sh/docs/intro/install/)
* [kubectl v1.18+](https://kubernetes.io/docs/reference/kubectl/)

All scripts and example Pod specs used in this demo are contained in this repository. Clone it and `cd` into it before starting:

```bash
git clone https://github.com/kubernetes-sigs/dra-driver-google-tpu.git
cd dra-driver-google-tpu
```

> [!NOTE]
> The scripts will automatically use either `docker` or `podman` as the container tool command, whichever is found in your PATH. To override this, set the `CONTAINER_TOOL` environment variable (e.g., `export CONTAINER_TOOL=docker`).

---

### Path A: Local Development with Kind

This path creates a local Kubernetes cluster using Kind and simulates TPU devices. It is ideal for testing the driver logic without needing real hardware.

#### 1. Build the Driver Image

Build the image locally:
```bash
make image-build
```

#### 2. Create the Kind Cluster

Run the script to create a Kind cluster with CDI support enabled. This script will also automatically load the image you just built into the cluster.
```bash
./demo/clusters/kind/create-cluster.sh
```

#### 3. Install the Driver

Install the driver components using Helm:
```bash
./demo/scripts/install-dra-driver.sh
```

Verify that the driver components have come up successfully:
```console
$ kubectl get pod -n dra-driver-google-tpu
NAME                                        READY   STATUS    RESTARTS   AGE
dra-driver-google-tpu-kubeletplugin-55jdj   3/3     Running   0          1m
```

And show the initial state of available TPU devices on the worker node:
```console
$ kubectl get resourceslice -o yaml
apiVersion: v1
items:
- apiVersion: resource.k8s.io/v1beta1
  kind: ResourceSlice
  metadata:
    creationTimestamp: "2025-01-21T18:49:28Z"
    generateName: kind-node-
    generation: 1
    name: kind-node-jh8t6
    resourceVersion: "3283457"
  spec:
    devices:
    - basic:
        attributes:
          index:
            int: 0
          tpuGen:
            string: v4
          uuid:
            string: tpu-3c4df1c1-ee02-ee8c-f699-29358f06d4dc
      name: accel0
    - basic:
        attributes:
          index:
            int: 1
          tpuGen:
            string: v4
          uuid:
            string: tpu-1a9a1690-6cdf-c5ac-a95c-b2896d950946
      name: accel1
    driver: tpu.google.com
    nodeName: kind-control-plane
    pool:
      generation: 1
      name: kind-control-plane
      resourceSliceCount: 1
kind: List
metadata:
  resourceVersion: ""
```
*(Note: The output above is truncated and simplified for illustration).*

#### 4. Run Demo Workload

Deploy a pod that requests fake TPU resources:
```bash
kubectl apply -f demo/specs/tpu-test.yaml
```

Verify that all pods are running successfully:
```bash
kubectl get pods -n tpu-test
```

Then verify that the TPU devices were correctly injected into the pod:
```bash
./demo/scripts/verify-tpu-devices.sh tpu-test
```

---

### Path B: Cloud Deployment with GKE

This path creates a GKE cluster with real TPU devices.

#### 1. Build and Push the Driver Image

You must build the image and push it to a container registry that your GKE cluster can access before installing the driver.

```bash
REGISTRY=my-registry.example.com make image-build
REGISTRY=my-registry.example.com make image-push
```

#### 2. Create the GKE Cluster

Use the script to create a GKE cluster with v6e TPUs (or any type for your specific needs) and prepare the cluster to be able to use DRA:

```bash
./demo/clusters/gke/create-tpu-cluster-for-dra.sh
```

#### 3. Install the Driver

If you used a custom registry when building the image, you must also pass it when running the install script:

```bash
REGISTRY=my-registry.example.com ./demo/scripts/install-dra-driver.sh
```

Verify the installation:
```bash
kubectl get pod -n dra-driver-google-tpu
kubectl get resourceslice -o yaml
```

#### 4. Run Demo Workload

> [!IMPORTANT]
> Before applying `vllm-tpu.yaml`, you must edit the file and replace `REPLACE_WITH_YOUR_HUGGING_FACE_TOKEN` with your actual Hugging Face token.

Deploy a pod that requests real TPU resources:
```bash
kubectl apply -f demo/specs/vllm-tpu.yaml
```

Verify that all pods are running successfully:
```bash
kubectl get pods -n tpu-test
```

Then verify that the TPU devices were correctly injected into the pod:
```bash
./demo/scripts/verify-tpu-devices.sh tpu-test
```

#### 5. Send a Test Request

Before sending a request, you must wait for the vLLM model serving server to initialize and load the model weights into the TPU memory (this may take a couple of minutes).

Monitor the logs until the server is ready and listening on port `8000`:
```bash
kubectl logs -l app=vllm-tpu --prefix -f -n tpu-test
```

You should see something like this once the server is fully ready:
```
(APIServer pid=1) INFO:     Started server process [1]
(APIServer pid=1) INFO:     Waiting for application startup.
(APIServer pid=1) INFO:     Application startup complete.
```

Once the server is ready, port-forward the service to your local machine in a separate terminal:
```bash
kubectl port-forward service/vllm-service 8000:8000 -n tpu-test
```

Then, in another terminal, send a test request to the model using `curl`:
```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen/Qwen2-1.5B",
    "messages": [
      {"role": "user", "content": "San Francisco is a"}
    ],
    "max_tokens": 50
  }'
```

---

### Path C: Vanilla Kubernetes on GCE

This path creates an upstream Kubernetes cluster with `kubeadm` on Compute
Engine VMs, with a TPU VM joined as a worker. Nothing in the cluster knows what a
TPU is: there is no cloud controller labelling the nodes and no TPU aware
scheduling, which makes it a good check that the driver works on any conformant
cluster.

The cluster is one control plane VM, one CPU worker and one single host TPU VM
(a `ct6e` Compute Engine instance). Nodes bootstrap themselves through
`cloud-init` and report progress through GCE guest attributes, so no SSH access
is needed.

Pods are addressed from a GCE alias IP range per node, carved out of a secondary
range of the subnet, so pod traffic is routed natively by the VPC with no overlay
and no cloud routes. The `cloud-controller-manager` from
[cloud-provider-gcp](https://github.com/kubernetes/cloud-provider-gcp) reads
those ranges back to set the node CIDRs (`--cidr-allocator-type=CloudAllocator`),
and brings Service type LoadBalancer and the zone/region topology labels with it.

```bash
PROJECT=my-project ZONE=us-central1-a ./demo/clusters/gce/create-kubeadm-tpu-cluster.sh
```

The script installs the driver, telling it only the accelerator type, which it
derives from the machine type:

```bash
./demo/scripts/install-dra-driver.sh --set kubeletPlugin.tpu.accelerator=tpu-v6e-slice
```

Everything else is discovered: the driver probes the chips and publishes a
ResourceSlice, which the script waits for before it returns. The other two nodes
run the same DaemonSet and stay idle.

#### What you should see

The node CIDRs are the alias ranges GCE handed out, not something allocated in
cluster. The script prints them before it installs the driver:

```console
$ kubectl get nodes -o custom-columns='NAME:.metadata.name,PODCIDR:.spec.podCIDR,PROVIDER:.spec.providerID'
NAME              PODCIDR         PROVIDER
tpudra-cp         10.244.0.0/24   gce://my-project/us-central1-a/tpudra-cp
tpudra-cpu        10.244.2.0/24   gce://my-project/us-central1-a/tpudra-cpu
tpudra-tpu        10.244.1.0/24   gce://my-project/us-central1-a/tpudra-tpu

$ gcloud compute instances describe tpudra-tpu --zone us-central1-a \
    --format="value(networkInterfaces[0].aliasIpRanges[0].ipCidrRange)"
10.244.1.0/24
```

The driver logs what it found. On a `ct6e-standard-1t`, one chip on the vfio
interface, with the topology derived from the chip count:

```console
$ kubectl logs -n dra-driver-google-tpu -l app.kubernetes.io/name=dra-driver-google-tpu -c tpu-dra-plugin
Found 1 TPU chips in /dev/vfio
Discovered TPU map[tpu.google.com/accelerator:tpu-v6e-slice] from driver configuration
Assuming the single host topology 1x1 for 1 TPU chips
```

The node without a TPU runs the same DaemonSet and stays idle instead of failing,
which is why the accelerator type can be set for the whole DaemonSet:

```console
No TPU detected on node "tpudra-cpu", retrying in 1m0s. Set --tpu-accelerator if this node does have a TPU.
```

Only the TPU node publishes a ResourceSlice, and its attributes are what a claim
selects on:

```console
$ kubectl get resourceslice -o yaml
    devices:
    - name: "0"
      attributes:
        accelerator: {string: tpu-v6e-slice}
        brand:       {string: Google}
        chipCount:   {int: 1}
        index:       {int: 0}
        topology:    {string: 1x1}
        tpuGen:      {string: v6e}
        uuid:        {string: tpu-e637a4fe-432f-c428-0e12-9ddd178359cc}
```
*(Abridged; `chipCount` and `topology` come from the hardware, `accelerator` from
the chart value.)*

Then run a workload that claims a TPU:
```bash
kubectl apply -f demo/specs/tpu-test.yaml
```

The chip is injected into the container and the TPU environment is set from what
was discovered:

```console
$ kubectl exec -n tpu-test tpu-pod0 -- ls -l /dev/vfio/
crw-rw-rw-    1 root     root      239,   0 /dev/vfio/0
crw-rw-rw-    1 root     root       10, 196 /dev/vfio/vfio

$ kubectl exec -n tpu-test tpu-pod0 -- env | grep ^TPU_ | sort
TPU_ACCELERATOR_TYPE=v6e-1
TPU_CHIPS_PER_HOST_BOUNDS=1,1,1
TPU_HOST_BOUNDS=1,1,1
TPU_SKIP_MDS_QUERY=true
TPU_TOPOLOGY=1x1
TPU_WORKER_HOSTNAMES=localhost
TPU_WORKER_ID=0
```

TPU VMs bill while they exist, so tear the cluster down when finished:
```bash
PROJECT=my-project ZONE=us-central1-a ./demo/clusters/gce/create-kubeadm-tpu-cluster.sh --delete
```

> [!NOTE]
> Every resource is named after `NAME_PREFIX` (`tpu-dra` by default), so set it
> to run more than one of these clusters in the same project. Installing a driver
> image from a private registry needs an image pull secret: a plain Ubuntu node
> has no credential helper for Artifact Registry.

---

## Running on any Kubernetes cluster

The driver does not depend on a specific cloud provider or on provider specific
node labels. It is deployed as a DaemonSet on every node, figures out whether the
node has a TPU and stays idle when it has none.

### TPU discovery

The driver probes the host for TPU chips: generations up to v4 expose one
`/dev/accel<n>` device per chip, later ones are bound to `vfio-pci` and expose
one `/dev/vfio/<iommu group>` device per chip, which the driver tells apart from
other VFIO devices by the Google PCI vendor id. The number of chips, the device
directory and, for a single host TPU, the topology are therefore read from the
hardware and never have to be labeled.

The accelerator type cannot be observed locally. It is resolved from the first
source that knows about it, in this order:

1. The driver configuration (`kubeletPlugin.tpu.*` Helm values).
2. The `tpu-env` file written by the Cloud TPU runtime on the host, `/etc/tpu/tpu-env` by default.
3. The Google Cloud metadata server, when running on GCE or GKE.
4. The labels already present on the `Node` object.

On a platform where none of the automatic sources is available, configure the
node explicitly. Only the accelerator type is required, a multi host slice also
needs its topology:

```bash
helm upgrade -i ... \
  --set kubeletPlugin.tpu.accelerator=tpu-v6e-slice
```

### Device attributes

What the driver discovers is published on the devices of the ResourceSlice, not
on the Node. Workloads select TPUs through the claim, so nothing has to label the
node and the driver needs no write access to it:

| Attribute | Example |
| --- | --- |
| `accelerator` | `tpu-v6e-slice` |
| `tpuGen` | `v6e` |
| `chipCount` | `8` |
| `topology` | `2x4` |
| `index` | `0` |
| `uuid` | `tpu-25541d5c-...` |
| `brand` | `Google` |

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: v6e-tpus
spec:
  spec:
    devices:
      requests:
      - name: tpus
        exactly:
          deviceClassName: tpu.google.com
          allocationMode: All
          selectors:
          - cel:
              expression: device.attributes["tpu.google.com"].accelerator == "tpu-v6e-slice"
```

The GKE node labels (`cloud.google.com/gke-tpu-accelerator` and friends) are
still read as an input when the platform sets them.

### Google Cloud specific containers

The `tpu-network-optimization` init container, the `sidecar-log-collector` and
the `vbar-control-agent` sidecars use images that are only published for Google
Cloud. Disable them on other platforms:

```bash
helm upgrade -i ... \
  --set kubeletPlugin.containers.networkOptimizer.enabled=false \
  --set kubeletPlugin.containers.logCollector.enabled=false \
  --set kubeletPlugin.containers.vbarControlAgent.enabled=false
```

---

## Tests

Unit tests:
```bash
make test
```

End to end tests, which create a kind cluster with fake TPU devices and drive the
scripts under `demo/`, see [tests/README.md](tests/README.md):
```bash
make test-e2e
```

---

## References

For more information on the DRA Kubernetes feature and developing custom resource drivers, see the following resources:

* [Dynamic Resource Allocation in Kubernetes](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
* [Example DRA Driver](https://github.com/kubernetes-sigs/dra-example-driver)

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack channel](https://kubernetes.slack.com/messages/sig-node)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/sig-node)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).
