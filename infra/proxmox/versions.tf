terraform {
  required_version = ">= 1.6"
  required_providers {
    proxmox = {
      source = "bpg/proxmox"
      # Pin the 0.66+ schema: clone{ vm_id }, initialization{ ip_config / user_account / dns },
      # disk{ serial } (the stable /dev/disk/by-id identity extra disks resolve through), and the
      # guest-agent ipv4_addresses attribute. Do not bump across a major without re-checking these.
      version = "~> 0.66"
    }
  }
}

# Credentials come from the environment, injected per-exec by internal/provision/proxmox:
#   PROXMOX_VE_ENDPOINT, PROXMOX_VE_INSECURE, and EITHER PROXMOX_VE_API_TOKEN (token auth) OR
#   PROXMOX_VE_USERNAME + PROXMOX_VE_PASSWORD (password auth).
# They are deliberately NOT variables - a workspace's terraform.tfvars.json persists on disk for
# the life of the cluster, and Proxmox credentials have no business being there (same stance as the
# vSphere module).
provider "proxmox" {}
