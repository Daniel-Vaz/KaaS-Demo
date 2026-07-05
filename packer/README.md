# Packer - golden images

Builds the **golden images** every cluster clones from: an Ubuntu cloud image with containerd and
pinned `kubeadm`/`kubelet`/`kubectl` pre-installed and the control-plane images pre-pulled. That's what
makes cluster creation a fast clone rather than an apt run - Ansible then only *forms* the cluster.

The software is baked by running the **same `ansible/roles/common` role** used at provision time (via
`ansible/playbooks/golden-image.yml`), so the image and the runtime path cannot drift.

```
packer/
  ubuntu-k8s.pkr.hcl   qemu source (KVM qcow2)      - the libvirt/KVM image
  vsphere/             vsphere-clone builder        - a vCenter VM template
  proxmox/             proxmox-clone builder        - a Proxmox VM template
  variables.pkr.hcl    k8s_version, os_name, base_image_url, output_dir, output_name, qemu_binary
```

A golden image is a function of **(OS, Kubernetes version)** - which a release bundle pins - so one is
baked per pair, named `<os>-k8s-<version>` (`.qcow2` on KVM; no suffix for the vSphere/Proxmox templates).

## Build

```bash
make golden-image                       # the default (ubuntu-26.04 / 1.36.2); skips if it exists
make golden-images                      # the shipped set for every configured provider
make golden-image-vsphere               # a vCenter template (needs KAAS_VSPHERE_* sourced)
make golden-image-proxmox               # a Proxmox template (needs KAAS_PROXMOX_* sourced)
```

The vSphere and Proxmox builds each need a one-time seed template imported by hand, and vSphere needs an
extra `cmd/vsphere-prep` step. The full instructions - seed templates, the datastore/latency caveat, and
building without `make` - are in [**Golden images**](../docs/deploy/golden-images.md).

## Use them

Point `KAAS_IMAGE_DIR` at the golden-image directory (bind-mounted into the worker in container mode);
the provisioner resolves each node's image from there by name, falling back to `KAAS_BASE_IMAGE`. See
the [operator guide](../docs/deploy/README.md).
