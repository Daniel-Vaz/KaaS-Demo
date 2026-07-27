terraform {
  required_version = ">= 1.6"
  required_providers {
    libvirt = {
      source = "dmacvicar/libvirt"
      # The 0.9 line is a ground-up rewrite of the provider (plugin-framework, HCL mapping ~1:1 onto
      # libvirt's domain/network/volume XML) and shares no schema with 0.8. main.tf is written against
      # it. Floored at the version actually exercised rather than opened to the whole 0.9 line, so
      # every later release still arrives as a Dependabot PR to look at instead of floating in
      # silently; the cap keeps a 0.10 out until someone reads it.
      version = "~> 0.9.8"
    }
  }
}

provider "libvirt" {
  uri = var.libvirt_uri
}
