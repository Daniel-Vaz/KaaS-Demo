# Ansible - configuration management

Turns the provisioned VMs into a `kubeadm` cluster and keeps forming, upgrading, and maintaining it.
Driven by `internal/config/ansible` with a **dynamic inventory generated per run from the database**
(never static). Base package install lives in the `common` role - a near no-op on a golden image, and
what lets a cluster form against a stock cloud image too.

Only `ansible.builtin` modules are used, so everything syntax-checks without extra Galaxy collections:

```bash
ANSIBLE_CONFIG=ansible/ansible.cfg ansible-playbook --syntax-check -i localhost, ansible/playbooks/bootstrap.yml
```

## What's here

```
ansible/
  playbooks/   one entry point per config-manager operation:
    bootstrap.yml            control-plane bring-up (single node or HA: LB → first CP → other CPs)
    join.yml                 mint a join command, then kubeadm join the workers (serves bring-up + scale-up)
    cni.yml                  install Cilium once, on the first control plane
    viewer-kubeconfig.yml    read-only RBAC + the viewer kubeconfig
    control-plane-metrics.yml  make etcd/scheduler/controller-manager/kube-proxy scrapeable
    node-disks.yml / remove-node-disks.yml   LVM-format + mount / tear down extra node disks
    longhorn-disks.yml / longhorn-evict.yml  register / drain extra Longhorn pool disks
    upgrade.yml              in-place kubeadm upgrade (Kubernetes minor bump)
    remove-worker.yml        drain + delete named nodes (scale-down)
    remove-controlplane.yml / join-controlplane.yml   HA control-plane rolling replacement (OS upgrade)
    backup-controlplane.yml / restore-controlplane.yml  single-node control-plane OS upgrade
    etcd-snapshot.yml / restore-etcd-snapshot.yml       periodic sealed backup + restore
    etcd-status.yml / etcd-defrag.yml                   etcd observation + defragmentation
    renew-certs.yml / check-cert-expiration.yml         certificate rotation
    repair-node.yml          restart containerd + kubelet (automatic repair, cheapest rung)
    golden-image.yml         bake node software into the golden image (run by Packer)
  roles/       the reusable units the playbooks compose (common, controlplane*, worker, cni,
               loadbalancer, node_disks, longhorn_disks, upgrade, renew_certs, etcd_maintenance,
               node_repair, controlplane_audit, controlplane_etcd, viewer_kubeconfig, external_secrets, …)
```

The Go wrapper passes the cluster's shape as extra-vars (`k8s_version`, `pod_cidr`, HA vars, disk
lists, upgrade targets, …) and reads the kubeconfig + join command back from the per-cluster artifacts
directory.

## More

- The full bring-up, HA, upgrade, and maintenance flows: [Architecture](../docs/concepts/architecture.md)
  and [Keeping clusters healthy](../docs/concepts/keeping-clusters-healthy.md).
- The deep rationale behind individual roles: [`CLAUDE.md`](../CLAUDE.md) and the roles themselves.
