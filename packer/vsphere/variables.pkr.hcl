# Variables for the vSphere golden-image build (vsphere-ubuntu-k8s.pkr.hcl). The connection and
# placement values default to the KAAS_VSPHERE_* environment - the same ones the worker uses - so
# `make golden-image-vsphere` works off a sourced .env with nothing else to pass.

variable "vsphere_server" {
  type        = string
  default     = env("KAAS_VSPHERE_SERVER")
  description = "vCenter host (no scheme), e.g. vcenter.example.internal. Defaults to $KAAS_VSPHERE_SERVER."
}

variable "vsphere_username" {
  type        = string
  default     = env("KAAS_VSPHERE_USERNAME")
  description = "vCenter user, e.g. DOMAIN\\user. Defaults to $KAAS_VSPHERE_USERNAME."
  sensitive   = true
}

variable "vsphere_password" {
  type        = string
  default     = env("KAAS_VSPHERE_PASSWORD")
  description = "vCenter password. Defaults to $KAAS_VSPHERE_PASSWORD."
  sensitive   = true
}

variable "vsphere_insecure" {
  type        = bool
  default     = true
  description = "Accept a self-signed vCenter certificate (the lab's)."
}

variable "vsphere_datacenter" {
  type        = string
  default     = env("KAAS_VSPHERE_DATACENTER")
  description = "Datacenter, e.g. MyDC. Defaults to $KAAS_VSPHERE_DATACENTER."
}

variable "vsphere_cluster" {
  type        = string
  default     = env("KAAS_VSPHERE_CLUSTER")
  description = "Compute cluster the build VM runs on, e.g. CLUSTER01. Defaults to $KAAS_VSPHERE_CLUSTER."
}

variable "vsphere_datastore" {
  type        = string
  default     = env("KAAS_VSPHERE_DATASTORE")
  description = "Datastore, e.g. datastorenl_002. Defaults to $KAAS_VSPHERE_DATASTORE."
}

variable "vsphere_folder" {
  type        = string
  default     = env("KAAS_VSPHERE_FOLDER")
  description = "VM folder holding the templates (and the per-cluster folders), e.g. DVaz. Defaults to $KAAS_VSPHERE_FOLDER."
}

variable "seed_template" {
  type        = string
  default     = "ubuntu-26.04-cloudimg-seed"
  description = "The stock Ubuntu cloud-image template to clone from (imported once with `govc import.ova`; see the build file's header). Must match os_name."
}

variable "k8s_version" {
  type        = string
  default     = "1.36.2"
  description = "Kubernetes version to bake (should match a catalog bundle's kubernetes)."
}

variable "os_name" {
  type        = string
  default     = "ubuntu-26.04"
  description = "Catalog OS name this image is for (should match a catalog os[].name)."
}

variable "output_name" {
  type        = string
  default     = "ubuntu-26.04-k8s-1.36.2"
  description = "Template name. Convention (catalog.GoldenImageNameFor for vsphere): <os_name>-k8s-<k8s_version>, no file suffix. `make golden-image-vsphere` computes it from OS_NAME/K8S_VERSION."
}
