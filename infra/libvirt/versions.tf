terraform {
  required_version = ">= 1.6"
  required_providers {
    libvirt = {
      source = "dmacvicar/libvirt"
      # Pin to the 0.8.x schema (source/format/base_volume_id, disk{} / network_interface{}
      # blocks, cloudinit attr). The 0.9.x line on the OpenTofu registry is an incompatible
      # ground-up rewrite (raw libvirt-XML style) - do not bump without rewriting main.tf.
      version = "~> 0.8.0"
    }
  }
}

provider "libvirt" {
  uri = var.libvirt_uri
}
