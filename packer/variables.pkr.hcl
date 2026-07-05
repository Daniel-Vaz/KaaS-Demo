variable "k8s_version" {
  type        = string
  default     = "1.36.2"
  description = "Kubernetes version to bake (should match a catalog bundle's kubernetes)."
}

variable "os_name" {
  type        = string
  default     = "ubuntu-26.04"
  description = "Catalog OS name this image is for (should match a catalog os[].name). Used with k8s_version to name the golden image; see catalog.GoldenImageName and the Makefile golden-image target."
}

variable "base_image_url" {
  type        = string
  default     = "https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.img"
  description = "Base Ubuntu cloud qcow2 (local path or URL) to bake on top of. Must match os_name (e.g. noble for ubuntu-24.04, resolute for ubuntu-26.04)."
}

variable "output_dir" {
  type        = string
  default     = "build"
  description = "Temp build directory Packer writes into; `make golden-image` wipes it each run and then moves the finished image to packer/output/."
}

variable "output_name" {
  type        = string
  default     = "ubuntu-26.04-k8s-1.36.2.qcow2"
  description = "Golden image filename. Convention (catalog.GoldenImageName): <os_name>-k8s-<k8s_version>.qcow2. `make golden-image` computes and passes this from OS_NAME/K8S_VERSION."
}

variable "qemu_binary" {
  type        = string
  default     = "/usr/libexec/qemu-kvm"
  description = "QEMU system emulator. WSL2/EL9 ships qemu-kvm, not qemu-system-x86_64."
}
