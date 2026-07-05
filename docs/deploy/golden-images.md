# Golden images

A **golden image** is a VM image with the node software already baked in - containerd, and pinned
`kubeadm` / `kubelet` / `kubectl`, with the control-plane images pre-pulled. Because everything is
baked, cluster creation is a fast clone and Ansible only has to *form* the cluster, not install
packages over apt.

An image is a function of exactly **(OS, Kubernetes version)**, which a [version
bundle](../portal/catalog.md) pins. You build one per pair, and the provisioner resolves each node's
image from that pair. Building the images per Kubernetes version is also what makes bundle upgrades
possible.

The build runs the **same `common` Ansible role** used at cluster-create time, so the baked image and
the runtime path can never drift - at create time `common` re-runs as a near no-op.

## The three artefacts

Each provider needs the image in its own form, all built from the same `golden-image.yml` playbook:

| Provider | Artefact | Name |
|---|---|---|
| libvirt / KVM | a qcow2 volume | `<os>-k8s-<version>.qcow2` |
| VMware vSphere | a vCenter VM **template** | `<os>-k8s-<version>` (no suffix) |
| Proxmox VE | a Proxmox VM **template** | `<os>-k8s-<version>` (no suffix) |

## libvirt / KVM

```bash
make golden-image        # the default: ubuntu-26.04 / k8s 1.36.2; skips if it already exists
make golden-images       # the whole shipped set (kvm + vSphere + Proxmox, if configured)
```

`make golden-image` computes the output name from `OS_NAME` / `K8S_VERSION` and skips the ~10-minute
KVM build if the image already exists; otherwise it builds and moves the finished qcow2 into
`GOLDEN_DEST` (default `/var/lib/libvirt/images`, libvirt's default pool dir - auto-elevating with
`sudo` if that dir is root-owned; override with `GOLDEN_DEST=packer/output` for local runs).

Build a different one, matching the base image URL to the OS:

```bash
make golden-image OS_NAME=ubuntu-24.04 K8S_VERSION=1.36.2 \
  BASE_IMAGE_URL=https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
```

Requires KVM and a QEMU system emulator; `packer/variables.pkr.hcl` defaults `qemu_binary` to
`/usr/libexec/qemu-kvm` (the WSL2/EL9 path) - override if yours differs.

Point `KAAS_IMAGE_DIR` at the golden-image directory (bind-mounted into the worker in container mode).
The provisioner looks up `<os>-k8s-<version>.qcow2` there and falls back to the single `KAAS_BASE_IMAGE`
for anything not present.

## VMware vSphere

```bash
source .env && make golden-image-vsphere OS_NAME=ubuntu-26.04 K8S_VERSION=1.36.2
```

This uses the `vsphere-clone` Packer builder and marks the result as a vCenter template. Connection and
placement come from the `KAAS_VSPHERE_*` environment (the same values the worker uses).

**One-time per OS, import a seed template.** Use the Ubuntu **OVA**, not the `.img` - it ships
`open-vm-tools`, which is what lets vCenter report a node's DHCP address back and lets cloud-init detect
the VMware datasource:

```bash
curl -O https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.ova
# The OVA declares a "VM Network" NIC that almost certainly doesn't exist in your vCenter -
# map it onto your portgroup in the import spec, or the import fails.
govc import.ova -ds=<datastore> -pool='<resource-pool>' -folder='<vm-folder>' \
  -options=import-spec.json resolute-server-cloudimg-amd64.ova
govc vm.markastemplate '<vm-folder>/ubuntu-26.04-cloudimg-seed'
```

> **Never boot the seed.** On first boot cloud-init marks the instance as configured, and the build's
> user-data is then ignored - the Packer build hangs waiting for SSH. Marking it a template makes that
> mistake impossible.

**Strip the inherited vApp config.** The Ubuntu OVA carries a vApp/OVF configuration that the vSphere
provider refuses to clone (and that would silently override cloud-init if satisfied with a CD-ROM). The
`make golden-image-vsphere` target removes it automatically via `cmd/vsphere-prep`; run it by hand
against an existing template with:

```bash
source .env && go run ./cmd/vsphere-prep -template ubuntu-26.04-k8s-1.36.2
```

See the [vSphere provider guide](providers/vsphere.md) for the datastore-latency caveat that matters
most here.

## Proxmox VE

```bash
source .env && make golden-image-proxmox OS_NAME=ubuntu-26.04 K8S_VERSION=1.36.2
```

The `proxmox-clone` builder additionally installs and enables **`qemu-guest-agent`**, which is
load-bearing: it's how Proxmox reports a node's DHCP address back and how cloud-init and DHCP mode work
at all. There's no post-build prep step - Proxmox has no inherited vApp config to strip.

**One-time per OS, create a seed template** on the node. Two things matter:

- It **must carry `qemu-guest-agent`** (bake it in with `virt-customize` before importing) - the Packer
  plugin discovers the build VM's IP through the guest agent and hangs without it.
- Its **disk must be grown** past the ~3.5 GB stock image - containerd + kube* + the pre-pulled images
  don't fit. Resize to 20 GB; cloud-init's growpart expands the filesystem on every clone.

```bash
wget https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.img
sudo virt-customize -a resolute-server-cloudimg-amd64.img --install qemu-guest-agent
qm create 9000 --name ubuntu-26.04-cloudimg-seed --memory 2048 --cores 2 \
    --net0 virtio,bridge=vmbr0 --scsihw virtio-scsi-single --agent enabled=1 --ostype l26
qm set 9000 --scsi0 <datastore>:0,import-from=$PWD/resolute-server-cloudimg-amd64.img
qm disk resize 9000 scsi0 20G
qm set 9000 --ide2 <datastore>:cloudinit --boot order=scsi0 --serial0 socket --vga serial0
qm template 9000
```

On a **static-only** node network (no DHCP), set `KAAS_PROXMOX_BUILD_IP` to a free address on the node
network so the build VM can reach the internet. See the [Proxmox provider guide](providers/proxmox.md).

## Building without `make`

Run Packer directly (PATH often shadows HashiCorp packer with cracklib's `/usr/sbin/packer`):

```bash
cd packer && /usr/bin/packer init . && /usr/bin/packer build \
  -var os_name=ubuntu-26.04 -var k8s_version=1.36.2 -var output_name=ubuntu-26.04-k8s-1.36.2.qcow2 .
```
