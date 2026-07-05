terraform {
  required_version = ">= 1.6"
  required_providers {
    vsphere = {
      source  = "hashicorp/vsphere"
      version = "~> 2.8"
    }
  }
}

# Credentials come from the environment (VSPHERE_SERVER / VSPHERE_USER / VSPHERE_PASSWORD /
# VSPHERE_ALLOW_UNVERIFIED_SSL), injected per-exec by internal/provision/vsphere. They are
# deliberately NOT variables: a workspace's terraform.tfvars.json persists on disk for the life of
# the cluster, and vCenter credentials have no business being there.
provider "vsphere" {}
