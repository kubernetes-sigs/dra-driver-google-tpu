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
            string: tpu-25541d5c-7c31-8412-d7cb-c8ebff2fa5c9
      name: accel0
    - basic:
        attributes:
          index:
            int: 1
          tpuGen:
            string: v4
          uuid:
            string: tpu-25541d5c-7c31-8412-d7cb-c8ebff2fa5c9
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
for pod in $(kubectl get pod --output=jsonpath='{.items[*].metadata.name}' -n tpu-test); do \
    for ctr in $(kubectl get pod ${pod} -o jsonpath='{.spec.containers[*].name}' -n tpu-test); do \
      echo "${pod} ${ctr}:"
      kubectl exec ${pod} -c ${ctr} -n tpu-test -- ls -l /dev/ | grep -E "accel|tpu" || echo "No TPU devices found"
    done
done
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
for pod in $(kubectl get pod --output=jsonpath='{.items[*].metadata.name}' -n tpu-test); do \
    for ctr in $(kubectl get pod ${pod} -o jsonpath='{.spec.containers[*].name}' -n tpu-test); do \
      echo "${pod} ${ctr}:"
      kubectl exec ${pod} -c ${ctr} -n tpu-test -- ls -l /dev/ | grep -E "accel|tpu" || echo "No TPU devices found"
    done
done
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

## Requesting TPUs via Extended Resources (KEP-5004)

Pods can request TPUs through the classic `resources.limits` API instead of a
ResourceClaim, using the [extended resource mapping](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#extended-resource)
on the DeviceClass. The scheduler translates the extended resource request into
a DRA allocation, so no workload changes are needed to migrate from a device
plugin to this driver.

This requires the `DRAExtendedResource` feature gate enabled on the
kube-apiserver, kube-scheduler, kube-controller-manager, and kubelet. The gate
is beta and on by default since Kubernetes 1.37; on 1.35/1.36 it must be
enabled explicitly. The Kind demo cluster config
(`demo/clusters/kind/scripts/kind-cluster-config.yaml`) enables it.

The implicit name `deviceclass.resource.kubernetes.io/tpu.google.com` is
always available. To also map a familiar explicit name such as
`google.com/tpu`, install the chart with:

```bash
helm upgrade -i --create-namespace --namespace dra-driver-google-tpu \
  dra-driver-google-tpu deployments/helm/dra-driver-google-tpu \
  --set deviceClass.extendedResourceName=google.com/tpu
```

or pass it through the install script:

```bash
EXTENDED_RESOURCE_NAME=google.com/tpu ./demo/scripts/install-dra-driver.sh
```

> [!WARNING]
> Do not set `google.com/tpu` on clusters where the classic TPU device plugin
> is also advertising that name — the two mechanisms would conflict.

Then deploy a pod that requests TPUs via `resources.limits`:

```bash
kubectl apply -f demo/specs/tpu-extended-resource.yaml
```

An end-to-end test of this path against a Kind cluster is available via
`make test-e2e-extended-resource` (see `test/e2e/extended-resource.sh`).

> [!WARNING]
> This driver only supports allocating all TPU chips on a node together, and an
> extended resource request cannot express `allocationMode: All`. The requested
> limit must equal the node's chip count (4 in the Kind demo cluster): on a
> node with more chips the scheduler binds a partial allocation that the
> kubelet plugin rejects, leaving the pod stuck in `ContainerCreating` until
> deleted.

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
