# karpenter-upcloud

A [Karpenter](https://karpenter.sh) cloud provider for [UpCloud](https://upcloud.com). It implements
`sigs.k8s.io/karpenter`'s `CloudProvider` interface against UpCloud's cloud server API, so Karpenter
can launch, consolidate and terminate UpCloud servers in response to pending pods.

Status: **alpha**. The API group is `karpenter.k8s.upcloud/v1alpha1` and will change.

## How it maps onto UpCloud

Karpenter's model was built around AWS, and several of its concepts have no direct UpCloud
equivalent. The choices below are the load-bearing ones — read them before filing a bug.

| Karpenter concept | UpCloud mapping |
| --- | --- |
| Instance type | Server **plan** (`2xCPU-4GB`, `HIMEM-4xCPU-32GB`, …), from `GET /plan` |
| Zone | UpCloud **zone** (`fi-hel1`), from `GET /zone` |
| Region | Also the UpCloud zone — see below |
| Offering price | `server_plan_<name>` from `GET /price`, converted from credits/hour to EUR/hour |
| Capacity type | Always `on-demand`; UpCloud has no spot, preemptible or reserved market |
| Instance tags | UpCloud **labels** (not UpCloud "tags", which are a separate account-level API) |
| Provider ID | `upcloud:////<server-uuid>` |

**Zone and region are the same value.** UpCloud exposes no grouping above a zone, and the
[UpCloud cloud-controller-manager](https://github.com/UpCloudLtd/upcloud-cloud-controller-manager)
sets both `topology.kubernetes.io/zone` and `topology.kubernetes.io/region` on the node to the zone
id. NodeClaims are labelled the same way; if they were not, Karpenter would never match a NodeClaim
to its Node.

**The provider ID has four slashes.** That is what the cloud-controller-manager writes
(`upcloud:////` + UUID), and Karpenter matches on the exact string. Do not "fix" it to
`upcloud://<uuid>`.

**Deleting a server takes two calls.** UpCloud refuses to delete a running server. `Delete` stops
the server and returns an error; Karpenter retries until the call reports `NodeClaimNotFound`, and
the retry that finds the server stopped deletes it along with its disks.

**Servers carry their own launch timestamp.** UpCloud's API reports no creation time for a server,
so the controller writes one into the `karpenter.sh/created-at` label. Garbage collection needs it
to tell a server launched seconds ago — whose NodeClaim has not been observed yet — from an orphan.

**Listing costs one call per server.** UpCloud's `GET /server` omits labels, and labels are how a
server is mapped back to its NodePool and NodeClass, so each server needs a details lookup. Results
are cached for a minute and the lookups run concurrently.

## Requirements

- A Kubernetes cluster running on UpCloud, with the
  [UpCloud cloud-controller-manager](https://github.com/UpCloudLtd/upcloud-cloud-controller-manager)
  installed. Karpenter relies on it to set `spec.providerID` and the topology labels on new nodes.
- Nodes must reach the control plane. Every node needs a private address, so a NodeClass must attach
  at least one `utility` or `private` interface.
- An UpCloud API token (preferred) or account credentials.

## Install

```bash
helm upgrade --install karpenter-crd oci://ghcr.io/kubekanvas/charts/karpenter-crd \
  --namespace karpenter --create-namespace
```

```bash
helm upgrade --install karpenter oci://ghcr.io/kubekanvas/charts/karpenter \
  --namespace karpenter \
  --set settings.clusterName=my-cluster \
  --set settings.clusterZone=fi-hel1 \
  --set credentials.existingSecret=upcloud-credentials
```

The credentials Secret holds either `UPCLOUD_TOKEN`, or `UPCLOUD_USERNAME` and `UPCLOUD_PASSWORD`:

```bash
kubectl create secret generic upcloud-credentials \
  --namespace karpenter --from-literal=UPCLOUD_TOKEN="$UPCLOUD_TOKEN"
```

`settings.clusterName` is written to every server as the `karpenter.sh/managed-by` label and is the
filter used to find them again. **Two clusters sharing an UpCloud account must not share a name**, or
each will garbage collect the other's nodes.

Then apply a NodeClass and a NodePool — see [`examples/v1alpha1`](examples/v1alpha1).

## UpCloudNodeClass

```yaml
apiVersion: karpenter.k8s.upcloud/v1alpha1
kind: UpCloudNodeClass
metadata:
  name: default
spec:
  zones: [fi-hel1, fi-hel2]
  storage:
    template: "01000000-0000-4000-8000-000030240200"  # Ubuntu 24.04
    size: 80
    tier: maxiops
  network:
    interfaces:
      - type: public
      - type: utility
  userData: |
    #cloud-config
    ...
```

| Field | Purpose |
| --- | --- |
| `zones` | Zones servers may launch into. Defaults to `--cluster-zone`. |
| `storage.template` | UUID of the template to clone. `upctl storage list --public --template` lists them. |
| `storage.size` / `.tier` / `.encrypted` | Root disk. Size defaults to the plan's allowance and can never be below the template's own size. |
| `userData` | cloud-init. **This is where the node joins the cluster** — without it a server boots and is garbage collected when it fails to register. |
| `loginUser` | Username and SSH keys for the initial account. |
| `network.interfaces` | Interfaces in order. Defaults to one public IPv4 plus utility. |
| `serverGroup` | UUID of a server group; use an anti-affinity group to spread nodes across hosts. |
| `labels` | Extra UpCloud labels. Keys are 2–32 printable ASCII characters and may not collide with the ones Karpenter manages. |
| `kubelet` | Used to compute allocatable capacity. Applying it to the node itself is `userData`'s job. |

### Well known labels

NodePools can select on any of these:

```
node.kubernetes.io/instance-type                     # plan name
topology.kubernetes.io/zone, topology.kubernetes.io/region
karpenter.k8s.upcloud/instance-cpu                   # cores
karpenter.k8s.upcloud/instance-memory                # MiB
karpenter.k8s.upcloud/instance-family                # general, dev, hicpu, himem, cloudnative, gpu
karpenter.k8s.upcloud/instance-storage-size          # GB included with the plan
karpenter.k8s.upcloud/instance-storage-tier          # maxiops, standard, hdd
karpenter.k8s.upcloud/instance-gpu-count
karpenter.k8s.upcloud/instance-gpu-model             # absent on non-GPU plans
karpenter.k8s.upcloud/instance-public-traffic-out
```

## Configuration

| Flag | Environment variable | Default | Purpose |
| --- | --- | --- | --- |
| `--cluster-name` | `CLUSTER_NAME` | — | **Required.** Labels and discovers this cluster's servers. |
| `--cluster-zone` | `CLUSTER_ZONE` | — | **Required.** Zone used when a NodeClass sets no `spec.zones`. |
| `--vm-memory-overhead-percent` | `VM_MEMORY_OVERHEAD_PERCENT` | `0.075` | Memory assumed lost to virtualisation until a real node reports its capacity. |
| `--disable-dry-run` | `DISABLE_DRY_RUN` | `false` | Skip validating NodeClass references against the UpCloud API. |
| — | `UPCLOUD_TOKEN` | — | API token. Mutually exclusive with the username/password pair. |
| — | `UPCLOUD_USERNAME` / `UPCLOUD_PASSWORD` | — | Account credentials. |
| — | `UPCLOUD_CLIENT_TIMEOUT` | `30s` | Per-request timeout, as a Go duration. |

Karpenter core's own options (`LOG_LEVEL`, `BATCH_MAX_DURATION`, `FEATURE_GATES`, …) apply as
documented upstream.

## Development

```bash
make build        # compile
make test         # unit tests
make lint         # golangci-lint
make generate     # regenerate deepcopy funcs and CRDs
make verify       # regenerate and fail if anything changed
make image        # build and publish with ko
```

`make generate` regenerates `zz_generated.deepcopy.go` and the `UpCloudNodeClass` CRD with
`controller-gen`, and re-vendors the Karpenter core CRDs from the pinned `sigs.k8s.io/karpenter`
version. The CRDs are vendored rather than fetched at runtime so that they can never drift from the
binary that reads them.

### Layout

```
cmd/controller           entrypoint
pkg/apis/v1alpha1        UpCloudNodeClass API types
pkg/cloudprovider        the CloudProvider implementation and drift detection
pkg/controllers          NodeClass status/hash/termination, NodeClaim GC and labelling, refreshers
pkg/operator             wiring and options
pkg/providers/instance   server create / get / list / delete
pkg/providers/instancetype  plans -> instance types, offerings
pkg/providers/pricing    the UpCloud price list
pkg/upcloud              the slice of the UpCloud SDK this provider uses
```

## Limitations

- No spot, reserved or preemptible capacity — UpCloud does not sell any.
- No `nodeClassRef` selectors for plans; narrow the catalogue with NodePool requirements instead.
- GPU plans advertise `nvidia.com/gpu`; the device plugin still has to be installed separately.
- Drift covers the NodeClass hash and the server's zone. A plan being retired by UpCloud is not
  reported as drift.

## License

Apache 2.0. See [LICENSE](LICENSE).
