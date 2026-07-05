// Command vsphere-prep strips the inherited vApp/OVF configuration from a golden VM template.
//
// Why this exists: the seed we clone is Ubuntu's cloud-image OVA, whose OVF descriptor declares a
// vApp config - six properties (user-data, hostname, public-keys, …) with
// ovfEnvironmentTransport=["iso"]. Every clone of it, including the golden template Packer bakes,
// inherits that config, and it breaks provisioning in two ways:
//
//  1. The Terraform vSphere provider refuses to clone such a template unless the VM declares a
//     client CD-ROM to carry the properties - "this virtual machine requires a client CDROM device
//     to deliver vApp properties". Our module declares none: it delivers cloud-init over guestinfo.
//
//  2. Worse, if we DID satisfy it with a CD-ROM, cloud-init would find an OVF environment on that
//     ISO and use its DataSourceOVF - which sorts ahead of DataSourceVMware in Ubuntu's
//     datasource_list - so it would boot with the template's EMPTY user-data instead of the
//     guestinfo document we inject: no kaas user, no SSH key, and in static mode no address. A
//     silent failure, discovered only when Ansible cannot log in.
//
// Removing the vApp config leaves exactly one cloud-init transport (guestinfo → DataSourceVMware),
// which is what infra/vsphere/main.tf targets. Idempotent: a template with no vApp config is left
// alone. Run by `make golden-image-vsphere` after Packer builds the template; see
// docs/infrastructure.md.
//
// A template cannot be reconfigured in place, so this converts it back to a VM, reconfigures it,
// and marks it as a template again.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func main() {
	name := flag.String("template", "", "template name inside KAAS_VSPHERE_FOLDER, e.g. ubuntu-26.04-k8s-1.36.2")
	flag.Parse()
	if *name == "" {
		log.Fatal("vsphere-prep: -template is required")
	}
	if err := run(context.Background(), *name); err != nil {
		log.Fatalf("vsphere-prep: %v", err)
	}
}

func run(ctx context.Context, name string) error {
	cfg, err := envConfig()
	if err != nil {
		return err
	}
	c, err := govmomi.NewClient(ctx, cfg.url, cfg.insecure)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", cfg.url.Host, err)
	}
	defer func() { _ = c.Logout(ctx) }()

	finder := find.NewFinder(c.Client, true)
	dc, err := finder.Datacenter(ctx, cfg.datacenter)
	if err != nil {
		return fmt.Errorf("datacenter %q: %w", cfg.datacenter, err)
	}
	finder.SetDatacenter(dc)

	vmPath := path.Join(cfg.folder, name)
	vm, err := finder.VirtualMachine(ctx, vmPath)
	if err != nil {
		return fmt.Errorf("template %q: %w", vmPath, err)
	}

	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"config.vAppConfig", "config.template"}, &props); err != nil {
		return fmt.Errorf("read %s: %w", vmPath, err)
	}
	if props.Config == nil || props.Config.VAppConfig == nil {
		fmt.Printf("vsphere-prep: %s has no vApp config - nothing to do\n", vmPath)
		return nil
	}
	isTemplate := props.Config.Template

	// A template cannot be reconfigured; convert it back to a VM first.
	if isTemplate {
		pool, err := finder.ResourcePool(ctx, fmt.Sprintf("/%s/host/%s/Resources", cfg.datacenter, cfg.cluster))
		if err != nil {
			return fmt.Errorf("resource pool for cluster %q: %w", cfg.cluster, err)
		}
		if err := vm.MarkAsVirtualMachine(ctx, *pool, nil); err != nil {
			return fmt.Errorf("convert template to VM: %w", err)
		}
	}

	task, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{
		VAppConfigRemoved: types.NewBool(true),
	})
	if err != nil {
		return fmt.Errorf("reconfigure: %w", err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("remove vApp config: %w", err)
	}

	if isTemplate {
		if err := vm.MarkAsTemplate(ctx); err != nil {
			return fmt.Errorf("re-mark as template: %w", err)
		}
	}
	fmt.Printf("vsphere-prep: removed the inherited vApp/OVF config from %s\n", vmPath)
	return nil
}

type config struct {
	url        *url.URL
	insecure   bool
	datacenter string
	cluster    string
	folder     string
}

// envConfig reads the same KAAS_VSPHERE_* environment the worker and the Packer build use.
func envConfig() (config, error) {
	var c config
	raw := os.Getenv("KAAS_VSPHERE_URL")
	if raw == "" {
		return c, fmt.Errorf("KAAS_VSPHERE_URL is required (source your .env)")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil {
		return c, fmt.Errorf("KAAS_VSPHERE_URL %q: %w", raw, err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/sdk"
	}
	user, pass := os.Getenv("KAAS_VSPHERE_USERNAME"), os.Getenv("KAAS_VSPHERE_PASSWORD")
	if user == "" || pass == "" {
		return c, fmt.Errorf("KAAS_VSPHERE_USERNAME and KAAS_VSPHERE_PASSWORD are required")
	}
	u.User = url.UserPassword(user, pass)

	c = config{
		url:        u,
		insecure:   os.Getenv("KAAS_VSPHERE_INSECURE") == "1",
		datacenter: os.Getenv("KAAS_VSPHERE_DATACENTER"),
		cluster:    os.Getenv("KAAS_VSPHERE_CLUSTER"),
		folder:     os.Getenv("KAAS_VSPHERE_FOLDER"),
	}
	if c.datacenter == "" || c.cluster == "" || c.folder == "" {
		return c, fmt.Errorf("KAAS_VSPHERE_DATACENTER, KAAS_VSPHERE_CLUSTER and KAAS_VSPHERE_FOLDER are required")
	}
	return c, nil
}
