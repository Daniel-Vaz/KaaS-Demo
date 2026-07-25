import { useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import {
  Title,
  Text,
  Stepper,
  Group,
  Button,
  Card,
  TextInput,
  Select,
  SegmentedControl,
  NumberInput,
  Switch,
  Stack,
  Alert,
  Badge,
  Divider,
  Progress,
  SimpleGrid,
  Anchor,
  Breadcrumbs,
  Grid,
  ThemeIcon,
  Box,
  RingProgress,
  Code,
} from '@mantine/core';
import { useForm } from '@mantine/form';
import { useMediaQuery } from '@mantine/hooks';
import {
  IconArrowLeft,
  IconArrowRight,
  IconRocket,
  IconInfoCircle,
  IconAlertTriangle,
  IconShieldCheck,
  IconServer2,
  IconStack2,
  IconNetwork,
  IconCpu,
  IconCircleCheck,
  IconCloud,
  IconServerBolt,
} from '@tabler/icons-react';
import { useCatalog, useCapacity, useCreateCluster } from '../lib/queries';
import { SIZES, LONGHORN_ADDON, DEFAULT_STORAGE_DISK_GB, MIN_DISK_GB, MAX_DISK_GB } from '../lib/types';
import type { NodePool, SizeSpec } from '../lib/types';
import { NodePoolEditor, poolsValid } from '../components/NodePoolEditor';
import { gib } from '../lib/format';
import { dedupeAddons } from '../lib/catalog';
import { isValidCidr, ipInSubnet, providerLabel } from '../lib/cluster';
import { addonMeta, CATEGORY_ORDER } from '../lib/addonMeta';
import { AddonValuesEditor } from '../components/AddonValuesEditor';
import { AddonValuesDiff } from '../components/AddonValuesDiff';
import { AddonCard } from '../components/AddonCard';
import { CustomAddonPicker } from '../components/CustomAddonPicker';
import type { Bundle, CustomAddonRef, CatalogAddon, ProviderInfo } from '../lib/types';

const HA_CONTROL_PLANES = 3;

function latestSupported(bundles: Bundle[]): string | undefined {
  const superseded = new Set(bundles.map((b) => b.supersedes).filter(Boolean));
  return bundles.find((b) => b.status === 'supported' && !superseded.has(b.name))?.name;
}

interface FormValues {
  name: string;
  bundle: string;
  size: string; // control-plane node size; workers are sized per pool
  nodePools: NodePool[]; // the cluster's worker pools; always includes "default" (the server ensures it)
  ha: boolean;
  addons: string[];
  addonValues: Record<string, string>; // add-on name -> edited Helm values YAML (only customized ones)
  customAddons: CustomAddonRef[]; // add-ons picked from the user's custom catalogs
  provider: string; // infrastructure to deploy on (see the catalog's providers)
  networkMode: 'auto' | 'custom'; // kvm only
  networkCidr: string; // kvm only
  storageDiskGB: number; // per-worker disk backing the cluster's default (Longhorn) StorageClass
  apiVip: string; // vsphere + HA + dhcp: the user picks the control-plane VIP
  loadBalancerIp: string; // vsphere/proxmox + dhcp: the user picks the default MetalLB pool / Envoy Gateway address
}

// The wizard's steps, in order. "infra" only exists when the deployment offers more than one
// infrastructure provider - with a single provider the choice is implicit, so asking would be
// noise. Steps are addressed by key rather than index precisely so that adding/removing one
// can't silently shift the others' navigation.
type StepKey = 'infra' | 'basics' | 'networking' | 'sizing' | 'addons' | 'review';

export function CreateCluster() {
  const navigate = useNavigate();
  const { data: catalog } = useCatalog();
  const { data: cap } = useCapacity();
  const create = useCreateCluster();
  const [step, setStep] = useState(0);
  // The labelled wizard steps don't fit horizontally on a phone; stack them vertically there.
  const verticalStepper = useMediaQuery('(max-width: 48em)');
  const [editingAddon, setEditingAddon] = useState<string | null>(null);
  const [diffingAddon, setDiffingAddon] = useState<string | null>(null);

  const providers = useMemo(() => catalog?.providers ?? [], [catalog]);
  const multiProvider = providers.length > 1;

  const form = useForm<FormValues>({
    initialValues: {
      name: '',
      bundle: '',
      size: 'small',
      // Every cluster starts with a "default" pool - the same shape a pool-unaware user would expect
      // (one worker group). The server ensures it too (app.ensureDefaultPool); seeding it here just
      // means the wizard shows it.
      nodePools: [{ name: 'default', size: 'small', desired_workers: 1 }],
      ha: false,
      storageDiskGB: DEFAULT_STORAGE_DISK_GB,
      addons: [],
      addonValues: {},
      customAddons: [],
      provider: '',
      networkMode: 'auto',
      networkCidr: '',
      apiVip: '',
      loadBalancerIp: '',
    },
    validate: {
      name: (v) => {
        if (!v.trim()) return 'Name is required';
        if (!/^[a-z0-9-]+$/.test(v)) return 'Lowercase letters, digits, and hyphens only';
        return null;
      },
      networkCidr: (v, values) =>
        values.provider === 'kvm' && values.networkMode === 'custom' && !isValidCidr(v)
          ? 'Enter a valid IPv4 CIDR with a /16–/28 prefix (e.g. 10.20.0.0/24)'
          : null,
    },
  });

  // The provider the user picked (or the deployment's only one), and its network shape.
  const provider = providers.find((p) => p.name === form.values.provider) ?? providers[0];
  // A shared-network provider (vSphere, Proxmox) sits on the operator's own subnet - signalled by
  // the presence of ip_mode - rather than a dedicated per-cluster network like KVM. Everything the
  // wizard does differently for such a provider keys on this, not on the specific provider name.
  const isSharedNet = !!provider?.ip_mode;
  // With an external DHCP server the platform has no address to hand out for the HA VIP: the subnet
  // is the operator's, and only they know what's free outside the DHCP pool. So the user supplies
  // it. (Static mode allocates it from the configured range, like KVM does.)
  const needsVip = isSharedNet && provider?.ip_mode === 'dhcp' && form.values.ha;
  const vipError =
    needsVip && provider?.network_cidr && form.values.apiVip.trim()
      ? ipInSubnet(form.values.apiVip.trim(), provider.network_cidr)
        ? null
        : `Enter a free host address inside ${provider.network_cidr}`
      : null;
  // metallb + envoy-gateway ship by default, so every cluster reserves one address for the default
  // MetalLB pool / Envoy Gateway. In dhcp mode the platform can't pick it (same reason as the VIP),
  // so the user supplies it - on EVERY dhcp cluster, not only HA ones. (Static/KVM allocate it.)
  const needsLb = isSharedNet && provider?.ip_mode === 'dhcp';
  const lbError =
    needsLb && provider?.network_cidr && form.values.loadBalancerIp.trim()
      ? ipInSubnet(form.values.loadBalancerIp.trim(), provider.network_cidr)
        ? null
        : `Enter a free host address inside ${provider.network_cidr}`
      : null;

  // Default the provider to the deployment's first (the only one, unless the Infrastructure step
  // is shown).
  useEffect(() => {
    if (!form.values.provider && providers[0]) form.setFieldValue('provider', providers[0].name);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [providers]);

  const bundles = catalog?.bundles ?? [];
  const optionalAddons = useMemo(
    () => dedupeAddons((catalog?.addons ?? []).filter((a) => a.type === 'addon')),
    [catalog],
  );

  // Group the optional add-ons by cosmetic category so the picker reads as sections rather than a
  // flat wall of checkboxes.
  const addonsByCategory = useMemo(() => {
    const m = new Map<string, CatalogAddon[]>();
    for (const a of optionalAddons) {
      const cat = addonMeta(a.name).category;
      (m.get(cat) ?? m.set(cat, []).get(cat)!).push(a);
    }
    return CATEGORY_ORDER.filter((c) => m.has(c)).map((c) => [c, m.get(c)!] as const);
  }, [optionalAddons]);

  // Default the bundle to the latest supported head, and preselect that bundle's add-ons.
  useEffect(() => {
    if (!catalog || form.values.bundle) return;
    const def = latestSupported(bundles) ?? bundles[0]?.name;
    if (def) {
      form.setFieldValue('bundle', def);
      const b = bundles.find((x) => x.name === def);
      const preset = optionalAddons.filter((a) => b && a.name in b.addons).map((a) => a.name);
      form.setFieldValue('addons', preset);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [catalog]);

  const selectedBundle = bundles.find((b) => b.name === form.values.bundle);

  // Local capacity preview mirroring internal/quota.ClusterUsage: nodes are NOT priced alike - the
  // control planes cost the cluster's own size, and each pool's workers cost that pool's size. An HA
  // cluster runs HA_CONTROL_PLANES control planes.
  const spec = SIZES[form.values.size] ?? SIZES.small;
  const controlPlanes = form.values.ha ? HA_CONTROL_PLANES : 1;
  const workers = form.values.nodePools.reduce((n, p) => n + Math.max(0, p.desired_workers), 0);
  const nodeCount = controlPlanes + workers;
  const poolCost = (pick: (s: SizeSpec) => number) =>
    form.values.nodePools.reduce(
      (sum, p) => sum + Math.max(0, p.desired_workers) * pick(SIZES[p.size] ?? SIZES.small),
      0,
    );
  const costVCPU = controlPlanes * spec.cpus + poolCost((s) => s.cpus);
  const costMemMB = controlPlanes * spec.memMB + poolCost((s) => s.memMB);
  // Disk: the control planes at the cluster's own size, and each pool's workers at its ROOT-DISK
  // OVERRIDE where it has one (that is what those nodes actually cost on the host's storage pool).
  // ...plus the per-worker storage disk that backs the default StorageClass. It is charged to the
  // owner's quota exactly like a root disk, so the preview has to include it or the wizard would
  // promise capacity admission will refuse.
  const storagePerWorker = form.values.addons.includes(LONGHORN_ADDON) ? form.values.storageDiskGB : 0;
  const costDiskGB =
    controlPlanes * spec.diskGB +
    form.values.nodePools.reduce(
      (sum, p) =>
        sum +
        Math.max(0, p.desired_workers) *
          ((p.disk_gb || (SIZES[p.size] ?? SIZES.small).diskGB) + storagePerWorker),
      0,
    );
  // Headroom is per-infrastructure: this cluster is admitted against the quota the user holds on
  // the provider they picked, not against some cross-provider total. Showing the sum here would
  // promise capacity the admission check will refuse.
  const providerCap = cap?.providers?.find((p) => p.provider === provider?.name);
  const remVCPU = providerCap ? providerCap.total_vcpu - providerCap.used_vcpu : undefined;
  const remMemMB = providerCap ? providerCap.total_mem_mb - providerCap.used_mem_mb : undefined;
  const remDiskGB = providerCap ? providerCap.total_disk_gb - providerCap.used_disk_gb : undefined;
  const overCPU = remVCPU !== undefined && costVCPU > remVCPU;
  const overMem = remMemMB !== undefined && costMemMB > remMemMB;
  // Disk is a real admission gate, so the preview has to price it - otherwise a cluster that fits in
  // cores and memory would look admissible right up until the server refuses it.
  const overDisk = remDiskGB !== undefined && costDiskGB > remDiskGB;
  const overBudget = overCPU || overMem || overDisk;

  const toggleAddon = (name: string) => {
    const on = form.values.addons.includes(name);
    form.setFieldValue(
      'addons',
      on ? form.values.addons.filter((n) => n !== name) : [...form.values.addons, name],
    );
  };

  // The steps this deployment actually has, and where we are in them. Navigation is by key, so
  // the presence of the Infrastructure step never shifts another step's logic.
  const stepKeys: StepKey[] = useMemo(
    () => [
      ...(multiProvider ? (['infra'] as StepKey[]) : []),
      'basics',
      'networking',
      'sizing',
      'addons',
      'review',
    ],
    [multiProvider],
  );
  const current = stepKeys[Math.min(step, stepKeys.length - 1)];
  const isLast = current === 'review';

  // canAdvance gates Continue on the current step's own validity. Steps with nothing to validate
  // (infra always has a selection; add-ons have defaults) simply pass. On vSphere the networking
  // step is pure deployment fact, so it has nothing to validate either - the VIP is validated on
  // the sizing step, where it's asked.
  const canAdvance = (): boolean => {
    if (current === 'basics') {
      return !form.validateField('name').hasError && !!form.values.bundle;
    }
    if (current === 'networking') {
      if (needsLb && (!form.values.loadBalancerIp.trim() || lbError)) {
        form.setFieldError('loadBalancerIp', lbError ?? 'A LoadBalancer address is required in DHCP mode');
        return false;
      }
      return isSharedNet || !form.validateField('networkCidr').hasError;
    }
    if (current === 'sizing') {
      if (!poolsValid(form.values.nodePools)) return false;
      if (needsVip && (!form.values.apiVip.trim() || vipError)) {
        form.setFieldError('apiVip', vipError ?? 'A control-plane VIP is required for an HA cluster');
        return false;
      }
    }
    return true;
  };

  const submit = () => {
    create.mutate(
      {
        name: form.values.name.trim(),
        bundle: form.values.bundle,
        size: form.values.size,
        node_pools: form.values.nodePools,
        ha: form.values.ha,
        addons: form.values.addons,
        // Only send overrides for add-ons that are actually selected.
        addon_values: Object.fromEntries(
          Object.entries(form.values.addonValues).filter(([name]) => form.values.addons.includes(name)),
        ),
        custom_addons: form.values.customAddons,
        provider: form.values.provider || undefined,
        // The node network is the provider's business: on vSphere it's deployment configuration
        // (the operator's portgroup), and only the HA VIP in dhcp mode is the user's to choose.
        network_cidr:
          !isSharedNet && form.values.networkMode === 'custom'
            ? form.values.networkCidr.trim()
            : undefined,
        // Explicit even when it equals the default, so the value the user saw in the preview is the
        // value that gets admitted. 0 (longhorn deselected) is meaningful, hence not omitted.
        storage_disk_gb: storagePerWorker,
        api_vip: needsVip ? form.values.apiVip.trim() : undefined,
        load_balancer_ip: needsLb ? form.values.loadBalancerIp.trim() : undefined,
      },
      { onSuccess: (c) => navigate(`/clusters/${c.id}`) },
    );
  };

  // Add-on counts for the summary. The bundle's `addons` map also contains the CNI (e.g. cilium),
  // which is installed separately and not a selectable add-on, so count only the pinned add-ons that
  // actually appear in the picker. Everything else selected is a user-chosen optional add-on.
  const bundlePinned = optionalAddons.filter((a) => selectedBundle?.addons[a.name]).map((a) => a.name);
  const bundleAddonCount = bundlePinned.length;
  const extraAddons = form.values.addons.filter((n) => !bundlePinned.includes(n));

  return (
    <Box maw={1400} mx="auto">
      <Breadcrumbs mb="xs">
        <Anchor component={Link} to="/clusters" size="sm">
          Clusters
        </Anchor>
        <Text size="sm" c="dimmed">
          New cluster
        </Text>
      </Breadcrumbs>
      <Group justify="space-between" align="flex-end" mb="lg">
        <div>
          <Title order={2}>Create a cluster</Title>
          <Text c="dimmed" size="sm" mt={4}>
            Provision a fresh Kubernetes cluster - pick a release bundle, size the nodes, and choose
            your add-ons.
          </Text>
        </div>
      </Group>

      <Grid gap="lg" align="flex-start">
        {/* Main wizard column */}
        <Grid.Col span={{ base: 12, lg: 8 }}>
          <Card radius="md" padding="xl">
            <Stepper
              active={step}
              onStepClick={setStep}
              size="sm"
              orientation={verticalStepper ? 'vertical' : 'horizontal'}
            >
              {/* Step 0 - Infrastructure. Only when the deployment offers a choice; React drops a
                  `false` child, so with one provider the Stepper is exactly as it was. */}
              {multiProvider && (
                <Stepper.Step label="Infrastructure" description="Where it runs">
                  <Stack mt="xl" gap="lg">
                    <div>
                      <Text size="sm" fw={600} mb={4}>
                        Infrastructure provider
                      </Text>
                      <Text size="xs" c="dimmed" mb={10}>
                        Where this cluster's VMs are provisioned. This is fixed for the life of the
                        cluster.
                      </Text>
                      <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
                        {providers.map((p) => (
                          <ProviderCard
                            key={p.name}
                            provider={p}
                            selected={form.values.provider === p.name}
                            onSelect={() => form.setFieldValue('provider', p.name)}
                          />
                        ))}
                      </SimpleGrid>
                    </div>
                  </Stack>
                </Stepper.Step>
              )}

              {/* Step 1 - Basics */}
              <Stepper.Step label="Basics" description="Name & release">
                <Stack mt="xl" gap="lg">
                  <TextInput
                    label="Cluster name"
                    description="Lowercase letters, digits, and hyphens"
                    placeholder="my-cluster"
                    withAsterisk
                    size="md"
                    {...form.getInputProps('name')}
                  />
                  <Select
                    label="Release bundle"
                    description="Pins a coherent OS + Kubernetes + CNI + add-on set"
                    withAsterisk
                    allowDeselect={false}
                    size="md"
                    data={bundles.map((b) => ({
                      value: b.name,
                      label: `${b.name} - k8s ${b.kubernetes} / ${b.os}${
                        b.status !== 'supported' ? ` (${b.status})` : ''
                      }`,
                    }))}
                    value={form.values.bundle}
                    onChange={(v) => {
                      if (!v) return;
                      form.setFieldValue('bundle', v);
                      const b = bundles.find((x) => x.name === v);
                      form.setFieldValue(
                        'addons',
                        optionalAddons.filter((a) => b && a.name in b.addons).map((a) => a.name),
                      );
                    }}
                  />
                  {selectedBundle && (
                    <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
                      <SpecTile label="Kubernetes" value={selectedBundle.kubernetes} color="brand" />
                      <SpecTile
                        label="CNI"
                        value={
                          selectedBundle.addons[selectedBundle.cni]
                            ? `${selectedBundle.cni} ${selectedBundle.addons[selectedBundle.cni]}`
                            : selectedBundle.cni
                        }
                        color="grape"
                      />
                      <SpecTile label="Operating system" value={selectedBundle.os} color="gray" />
                    </SimpleGrid>
                  )}
                </Stack>
              </Stepper.Step>

              {/* Step 2 - Networking. On vSphere the node network is the operator's shared
                  portgroup (deployment configuration, not a per-cluster choice), so there is
                  nothing to choose here at all - the one address the user does supply, the HA
                  control-plane VIP, is asked for on the Sizing step, where HA is turned on. */}
              <Stepper.Step label="Networking" description="Node network">
                <Stack mt="xl" gap="lg">
                  {isSharedNet ? (
                    <div>
                      <Text size="sm" fw={600} mb={4}>
                        Node network
                      </Text>
                      <Text size="xs" c="dimmed" mb={10}>
                        vSphere clusters share the network configured for this deployment.
                      </Text>
                      <SimpleGrid cols={{ base: 1, sm: provider?.ip_mode === 'static' && provider?.net_range ? 4 : 3 }} spacing="sm">
                        <SpecTile label="Portgroup" value={provider?.network_name ?? '-'} color="brand" />
                        <SpecTile label="Subnet" value={provider?.network_cidr ?? '-'} color="gray" />
                        <SpecTile
                          label="Address assignment"
                          value={provider?.ip_mode === 'static' ? 'static (platform-allocated)' : 'DHCP'}
                          color="grape"
                        />
                        {provider?.ip_mode === 'static' && provider?.net_range && (
                          <SpecTile label="Allocation range" value={provider.net_range} color="teal" />
                        )}
                      </SimpleGrid>
                      {isSharedNet && provider?.ip_mode === 'static' && (
                        <Alert
                          mt="md"
                          variant="light"
                          color="blue"
                          icon={<IconInfoCircle size={16} />}
                          radius="md"
                        >
                          Node addresses (and the HA VIP + the default LoadBalancer IP) are allocated
                          from this deployment's configured range
                          {provider?.net_range ? (
                            <>
                              {' '}
                              (<Code>{provider.net_range}</Code>)
                            </>
                          ) : (
                            '.'
                          )}
                        </Alert>
                      )}
                      {/* metallb + envoy-gateway ship by default, so every cluster reserves one
                          address for the default MetalLB pool / Envoy Gateway. In dhcp mode the
                          platform can't pick it (the subnet is the operator's), so the user does -
                          on every dhcp cluster, HA or not. */}
                      {needsLb && (
                        <TextInput
                          mt="md"
                          label="LoadBalancer IP"
                          description={`A free address in ${provider?.network_cidr ?? 'the subnet'}, outside the DHCP pool. MetalLB advertises it (L2/ARP) and the default Envoy Gateway serves on it.`}
                          placeholder="172.23.252.230"
                          withAsterisk
                          value={form.values.loadBalancerIp}
                          error={form.errors.loadBalancerIp ?? lbError}
                          onChange={(e) => {
                            form.setFieldValue('loadBalancerIp', e.currentTarget.value);
                            form.clearFieldError('loadBalancerIp');
                          }}
                        />
                      )}
                    </div>
                  ) : (
                    <div>
                      <Text size="sm" fw={600} mb={4}>
                        Node network
                      </Text>
                      <Text size="xs" c="dimmed" mb={10}>
                        Each cluster runs on its own dedicated, isolated NAT bridge on KVM. Auto picks a
                        free block; choose Custom to set the CIDR (and size) yourself.
                      </Text>
                      <SegmentedControl
                        fullWidth
                        data={[
                          { value: 'auto', label: 'Auto-allocate' },
                          { value: 'custom', label: 'Custom CIDR' },
                        ]}
                        value={form.values.networkMode}
                        onChange={(v) => form.setFieldValue('networkMode', v as 'auto' | 'custom')}
                      />
                      {form.values.networkMode === 'custom' && (
                        <TextInput
                          mt="md"
                          label="Network CIDR"
                          description="IPv4, /16–/28 (e.g. 10.20.0.0/24). Must not overlap another cluster or a reserved range."
                          placeholder="10.20.0.0/24"
                          {...form.getInputProps('networkCidr')}
                        />
                      )}
                    </div>
                  )}
                </Stack>
              </Stepper.Step>

              {/* Step 3 - Sizing */}
              <Stepper.Step label="Sizing" description="Control plane, node pools, HA">
                <Stack mt="xl" gap="lg">
                  <div>
                    <Text size="sm" fw={600} mb={8}>
                      Control plane size
                    </Text>
                    <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
                      {Object.keys(SIZES).map((s) => (
                        <SizeCard
                          key={s}
                          name={s}
                          selected={form.values.size === s}
                          onSelect={() => form.setFieldValue('size', s)}
                        />
                      ))}
                    </SimpleGrid>
                  </div>
                  <Switch
                    size="md"
                    label="Highly-available control plane"
                    description="3 stacked-etcd control planes behind a VIP"
                    thumbIcon={form.values.ha ? <IconShieldCheck size={12} /> : undefined}
                    {...form.getInputProps('ha', { type: 'checkbox' })}
                  />

                  <div>
                    <Text size="sm" fw={600} mb={4}>
                      Node pools
                    </Text>
                    <Text size="xs" c="dimmed" mb={10}>
                      Worker nodes live in pools - each a group of one size, scaled independently
                      after creation. Nodes are named{' '}
                      <code>{(form.values.name || 'cluster') + '-<pool>-<n>'}</code> and carry a{' '}
                      <code>kaas.io/nodepool</code> label, so workloads can target a pool with a
                      nodeSelector.
                    </Text>
                    <NodePoolEditor
                      pools={form.values.nodePools}
                      onChange={(pools) => form.setFieldValue('nodePools', pools)}
                      controlPlaneSize={form.values.size}
                    />
                  </div>

                  {/* Storage sits here rather than on the Add-ons step because it is a SIZE - it is
                      charged to the same quota as the nodes above and shows up in the capacity
                      preview below. The field disappears entirely when longhorn is deselected: with
                      no default StorageClass to back, the disks would be capacity nobody uses. */}
                  {form.values.addons.includes(LONGHORN_ADDON) && (
                    <NumberInput
                      label="Storage per worker"
                      description="Each worker gets an extra disk of this size, mounted at /var/lib/longhorn and used by Longhorn to back the cluster's default StorageClass - so a PersistentVolumeClaim just works. Fixed after creation; attach more disks to a node later to grow its share."
                      suffix=" GB"
                      min={MIN_DISK_GB}
                      max={MAX_DISK_GB}
                      step={10}
                      clampBehavior="strict"
                      value={form.values.storageDiskGB}
                      onChange={(v) =>
                        form.setFieldValue(
                          'storageDiskGB',
                          typeof v === 'number' ? v : DEFAULT_STORAGE_DISK_GB,
                        )
                      }
                    />
                  )}

                  {/* The VIP is the one address the platform can't pick on vSphere with an
                      external DHCP server: the subnet is the operator's, and only they know what's
                      free outside the DHCP pool. It belongs here, next to the toggle that creates
                      the need for it - an HA control plane is the only thing a VIP is for. */}
                  {needsVip && (
                    <TextInput
                      label="Control-plane VIP"
                      description={`A free address in ${provider?.network_cidr ?? 'the subnet'}, outside the DHCP pool. keepalived floats it across the 3 control planes to give the API server one stable endpoint.`}
                      placeholder="172.23.252.240"
                      withAsterisk
                      value={form.values.apiVip}
                      error={form.errors.apiVip ?? vipError}
                      onChange={(e) => {
                        form.setFieldValue('apiVip', e.currentTarget.value);
                        form.clearFieldError('apiVip');
                      }}
                    />
                  )}

                  <Divider label="Capacity preview" labelPosition="left" />
                  <CapacityPreview
                    nodeCount={nodeCount}
                    controlPlanes={controlPlanes}
                    workers={workers}
                    costVCPU={costVCPU}
                    costMemMB={costMemMB}
                    costDiskGB={costDiskGB}
                    remVCPU={remVCPU}
                    remMemMB={remMemMB}
                    remDiskGB={remDiskGB}
                    overCPU={overCPU}
                    overMem={overMem}
                    overDisk={overDisk}
                  />
                </Stack>
              </Stepper.Step>

              {/* Step 4 - Add-ons */}
              <Stepper.Step label="Add-ons" description="Optional software">
                <Stack mt="xl" gap="lg">
                  <Alert variant="light" color="blue" icon={<IconInfoCircle size={16} />} radius="md">
                    The bundle's add-ons are preselected and locked - they ship with the bundle, so
                    they can't be disabled here. The CNI ({selectedBundle?.cni}) is always installed
                    too. You can change non-bundle add-ons anytime from the cluster's Add-ons tab.
                  </Alert>

                  {optionalAddons.length === 0 && (
                    <Text c="dimmed" size="sm">
                      No optional add-ons in the catalog.
                    </Text>
                  )}

                  {addonsByCategory.map(([category, items]) => (
                    <div key={category}>
                      <Text
                        size="xs"
                        fw={700}
                        c="dimmed"
                        tt="uppercase"
                        mb="xs"
                        style={{ letterSpacing: '0.04em' }}
                      >
                        {category}
                      </Text>
                      <SimpleGrid cols={{ base: 1, sm: 2, xl: 3 }} spacing="sm">
                        {items.map((a) => {
                          const pinned = selectedBundle?.addons[a.name];
                          const meta = addonMeta(a.name);
                          return (
                            <AddonCard
                              key={a.name}
                              name={a.name}
                              version={pinned ?? a.version}
                              description={a.description}
                              icon={meta.icon}
                              color={meta.color}
                              locked={!!pinned}
                              selected={form.values.addons.includes(a.name)}
                              edited={!!form.values.addonValues[a.name]}
                              onToggle={() => toggleAddon(a.name)}
                              onEditValues={() => setEditingAddon(a.name)}
                              onViewDiff={() => setDiffingAddon(a.name)}
                            />
                          );
                        })}
                      </SimpleGrid>
                    </div>
                  ))}

                  <Divider label="Custom catalog add-ons" labelPosition="left" />
                  <CustomAddonPicker
                    selected={form.values.customAddons}
                    onChange={(next) => form.setFieldValue('customAddons', next)}
                  />
                </Stack>
              </Stepper.Step>

              {/* Step 5 - Review */}
              <Stepper.Step label="Review" description="Confirm & create">
                <Stack mt="xl" gap="xs">
                  <ReviewRow label="Name" value={form.values.name || '-'} />
                  {multiProvider && (
                    <ReviewRow label="Infrastructure" value={providerLabel(form.values.provider)} />
                  )}
                  <ReviewRow label="Bundle" value={form.values.bundle} />
                  <ReviewRow label="Kubernetes" value={selectedBundle?.kubernetes ?? '-'} />
                  <ReviewRow
                    label="Control plane"
                    value={form.values.ha ? `HA · ${HA_CONTROL_PLANES} nodes` : 'single node'}
                  />
                  <ReviewRow label="Control plane" value={`${form.values.size} · ${controlPlanes} node(s)`} />
                  <ReviewRow
                    label="Node pools"
                    value={
                      form.values.nodePools.length === 0
                        ? 'none - control plane only'
                        : form.values.nodePools
                            .map((p) => `${p.name}: ${p.desired_workers} × ${p.size}`)
                            .join(', ')
                    }
                  />
                  <ReviewRow
                    label="Node network"
                    value={
                      isSharedNet
                        ? `${provider?.network_name ?? 'vsphere'} · ${provider?.network_cidr ?? ''} (${provider?.ip_mode ?? 'dhcp'}${
                            provider?.ip_mode === 'static' && provider?.net_range
                              ? `, range ${provider.net_range}`
                              : ''
                          })`
                        : form.values.networkMode === 'custom'
                          ? form.values.networkCidr || '-'
                          : 'auto-allocated'
                    }
                  />
                  {needsVip && <ReviewRow label="Control-plane VIP" value={form.values.apiVip || '-'} />}
                  {needsLb && (
                    <ReviewRow label="LoadBalancer IP" value={form.values.loadBalancerIp || '-'} />
                  )}
                  <ReviewRow
                    label="Add-ons"
                    value={form.values.addons.length ? form.values.addons.join(', ') : 'bundle defaults'}
                  />
                  {form.values.customAddons.length > 0 && (
                    <ReviewRow
                      label="Custom add-ons"
                      value={form.values.customAddons.map((r) => r.name).join(', ')}
                    />
                  )}
                  {Object.keys(form.values.addonValues).filter((n) => form.values.addons.includes(n)).length > 0 && (
                    <ReviewRow
                      label="Customized values"
                      value={Object.keys(form.values.addonValues)
                        .filter((n) => form.values.addons.includes(n))
                        .join(', ')}
                    />
                  )}
                  <ReviewRow label="Total nodes" value={String(nodeCount)} />
                  {overBudget && (
                    <Alert color="red" icon={<IconAlertTriangle size={16} />} mt="sm">
                      This exceeds available host capacity and will be rejected by the quota check.
                    </Alert>
                  )}
                </Stack>
              </Stepper.Step>
            </Stepper>

            <Group justify="space-between" mt="xl">
              <Button
                variant="default"
                leftSection={<IconArrowLeft size={16} />}
                onClick={() => (step === 0 ? navigate('/clusters') : setStep((s) => s - 1))}
              >
                {step === 0 ? 'Cancel' : 'Back'}
              </Button>
              {!isLast ? (
                <Button
                  rightSection={<IconArrowRight size={16} />}
                  onClick={() => {
                    if (canAdvance()) setStep((s) => s + 1);
                  }}
                >
                  Continue
                </Button>
              ) : (
                <Button
                  color="teal"
                  leftSection={<IconRocket size={16} />}
                  loading={create.isPending}
                  disabled={overBudget}
                  onClick={submit}
                >
                  Create cluster
                </Button>
              )}
            </Group>
          </Card>
        </Grid.Col>

        {/* Live summary sidebar */}
        <Grid.Col span={{ base: 12, lg: 4 }}>
          <Box style={{ position: 'sticky', top: 16 }}>
            <SummarySidebar
              name={form.values.name}
              bundle={form.values.bundle}
              kubernetes={selectedBundle?.kubernetes}
              cni={selectedBundle?.cni}
              ha={form.values.ha}
              controlPlanes={controlPlanes}
              size={form.values.size}
              nodePools={form.values.nodePools}
              workers={workers}
              nodeCount={nodeCount}
              provider={multiProvider ? providerLabel(form.values.provider) : undefined}
              network={
                isSharedNet
                  ? (provider?.network_name ?? 'vsphere')
                  : form.values.networkMode === 'custom'
                    ? form.values.networkCidr || 'custom'
                    : 'auto-allocated'
              }
              bundleAddons={bundleAddonCount}
              extraAddons={extraAddons.length}
              customAddons={form.values.customAddons.length}
              costVCPU={costVCPU}
              costMemMB={costMemMB}
              costDiskGB={costDiskGB}
              remVCPU={remVCPU}
              remMemMB={remMemMB}
              remDiskGB={remDiskGB}
              overBudget={overBudget}
            />
          </Box>
        </Grid.Col>
      </Grid>

      {editingAddon && (
        <AddonValuesEditor
          opened
          onClose={() => setEditingAddon(null)}
          addonName={editingAddon}
          addonVersion={selectedBundle?.addons[editingAddon] ?? optionalAddons.find((a) => a.name === editingAddon)?.version}
          bundle={form.values.bundle}
          initialOverride={form.values.addonValues[editingAddon]}
          onSaveDraft={(override) => {
            const next = { ...form.values.addonValues };
            if (override === null) delete next[editingAddon];
            else next[editingAddon] = override;
            form.setFieldValue('addonValues', next);
          }}
        />
      )}

      {diffingAddon && (
        <AddonValuesDiff
          opened
          onClose={() => setDiffingAddon(null)}
          addonName={diffingAddon}
          addonVersion={selectedBundle?.addons[diffingAddon] ?? optionalAddons.find((a) => a.name === diffingAddon)?.version}
          bundle={form.values.bundle}
          override={form.values.addonValues[diffingAddon] ?? ''}
        />
      )}
    </Box>
  );
}

// SpecTile is a small labelled fact used to surface a chosen bundle's coordinates.
function SpecTile({ label, value, color }: { label: string; value: string; color: string }) {
  return (
    <Card padding="sm" radius="md" withBorder={false} bg="var(--mantine-color-default-hover)">
      <Text size="xs" c="dimmed" tt="uppercase" fw={600} style={{ letterSpacing: '0.03em' }}>
        {label}
      </Text>
      <Text size="sm" fw={600} c={color === 'gray' ? undefined : `${color}.4`} mt={2} truncate>
        {value}
      </Text>
    </Card>
  );
}

// SizeCard is the interactive node-size selector, replacing a flat segmented control with tiles
// that spell out each t-shirt size's vCPU / memory / disk.
function SizeCard({
  name,
  selected,
  onSelect,
}: {
  name: string;
  selected: boolean;
  onSelect: () => void;
}) {
  const spec = SIZES[name];
  return (
    <Card
      padding="md"
      radius="md"
      onClick={onSelect}
      style={{
        cursor: 'pointer',
        borderColor: selected ? 'var(--mantine-color-brand-6)' : undefined,
        background: selected ? 'var(--mantine-color-brand-light)' : undefined,
      }}
    >
      <Group justify="space-between" mb={6}>
        <Group gap={8}>
          <ThemeIcon size={28} radius="md" variant={selected ? 'filled' : 'light'} color="brand">
            <IconCpu size={16} />
          </ThemeIcon>
          <Text fw={700} tt="capitalize">
            {name}
          </Text>
        </Group>
        {selected && <IconCircleCheck size={18} color="var(--mantine-color-brand-6)" />}
      </Group>
      <Text size="xs" c="dimmed">
        {spec.cpus} vCPU · {gib(spec.memMB)} GB RAM
      </Text>
      <Text size="xs" c="dimmed">
        {spec.diskGB} GB disk
      </Text>
    </Card>
  );
}

// ProviderCard picks the infrastructure a cluster is provisioned on. It shows the network the
// cluster's nodes will land on, because on a shared operator network (vSphere, Proxmox) that is the
// part with real consequences for the user - it's where their nodes become reachable.
const PROVIDER_ICON: Record<string, ReactNode> = {
  vsphere: <IconCloud size={16} />,
  proxmox: <IconServer2 size={16} />,
  kvm: <IconServerBolt size={16} />,
};
function ProviderCard({
  provider,
  selected,
  onSelect,
}: {
  provider: ProviderInfo;
  selected: boolean;
  onSelect: () => void;
}) {
  // A shared operator network is signalled by ip_mode (vSphere, Proxmox); KVM has none.
  const shared = !!provider.ip_mode;
  return (
    <Card
      padding="md"
      radius="md"
      onClick={onSelect}
      style={{
        cursor: 'pointer',
        borderColor: selected ? 'var(--mantine-color-brand-6)' : undefined,
        background: selected ? 'var(--mantine-color-brand-light)' : undefined,
      }}
    >
      <Group justify="space-between" mb={6}>
        <Group gap={8}>
          <ThemeIcon size={28} radius="md" variant={selected ? 'filled' : 'light'} color="brand">
            {PROVIDER_ICON[provider.name] ?? <IconServerBolt size={16} />}
          </ThemeIcon>
          <Text fw={700}>{providerLabel(provider.name)}</Text>
        </Group>
        {selected && <IconCircleCheck size={18} color="var(--mantine-color-brand-6)" />}
      </Group>
      <Text size="xs" c="dimmed">
        {shared
          ? `${provider.network_name ?? 'network'} · ${provider.network_cidr ?? ''}`
          : 'QEMU/KVM VMs on an isolated per-cluster network'}
      </Text>
      {shared && (
        <Text size="xs" c="dimmed">
          {provider.ip_mode === 'static' ? 'Static addressing' : 'DHCP addressing'}
        </Text>
      )}
    </Card>
  );
}

// SummarySidebar is the persistent live recap of the wizard's current selections plus a compact
// capacity read-out. It stays visible on every step so the running configuration is always in view.
function SummarySidebar(props: {
  name: string;
  bundle: string;
  kubernetes?: string;
  cni?: string;
  ha: boolean;
  controlPlanes: number;
  size: string; // control-plane node size
  nodePools: NodePool[]; // worker pools, each at its own t-shirt size
  workers: number;
  nodeCount: number;
  provider?: string; // omitted when the deployment has only one infrastructure provider
  network: string;
  bundleAddons: number;
  extraAddons: number;
  customAddons: number;
  costVCPU: number;
  costMemMB: number;
  costDiskGB: number;
  remVCPU?: number;
  remMemMB?: number;
  remDiskGB?: number;
  overBudget: boolean;
}) {
  const {
    name,
    bundle,
    kubernetes,
    ha,
    controlPlanes,
    size,
    nodePools,
    workers,
    nodeCount,
    provider,
    network,
    bundleAddons,
    extraAddons,
    customAddons,
    costVCPU,
    costMemMB,
    costDiskGB,
    remVCPU,
    remMemMB,
    remDiskGB,
    overBudget,
  } = props;

  const totalAddons = bundleAddons + extraAddons + customAddons;

  return (
    <Card radius="md" padding="lg">
      <Group gap="xs" mb="md">
        <ThemeIcon size={30} radius="md" variant="light" color="brand">
          <IconServer2 size={17} />
        </ThemeIcon>
        <div>
          <Text fw={600} size="sm" lh={1.1}>
            {name || 'New cluster'}
          </Text>
          <Text size="xs" c="dimmed" lh={1.1}>
            {bundle || 'no bundle'}
          </Text>
        </div>
      </Group>

      <Stack gap={10}>
        <SummaryRow
          icon={<IconStack2 size={15} />}
          label="Kubernetes"
          value={kubernetes ?? '-'}
        />
        <SummaryRow
          icon={<IconShieldCheck size={15} />}
          label="Control plane"
          value={ha ? `HA · ${controlPlanes} × ${size}` : `1 × ${size}`}
        />
        {/* Workers are NOT uniformly sized - each pool carries its own t-shirt size, so a single
            "N × size" line would misreport a multi-pool cluster. Show the running total on the row
            and break the composition out per pool below it. */}
        <div>
          <SummaryRow
            icon={<IconServer2 size={15} />}
            label="Workers"
            value={String(workers)}
          />
          {nodePools.length > 0 && (
            <Stack gap={2} mt={4} ml={23}>
              {nodePools.map((p) => (
                <Group key={p.name} justify="space-between" wrap="nowrap" gap="sm">
                  <Text size="xs" c="dimmed" truncate>
                    {p.name}
                  </Text>
                  <Text size="xs" c="dimmed" ta="right" style={{ whiteSpace: 'nowrap' }}>
                    {Math.max(0, p.desired_workers)} × {p.size}
                  </Text>
                </Group>
              ))}
            </Stack>
          )}
        </div>
        {provider && (
          <SummaryRow icon={<IconCloud size={15} />} label="Infrastructure" value={provider} />
        )}
        <SummaryRow icon={<IconNetwork size={15} />} label="Network" value={network} />
        <SummaryRow
          icon={<IconCpu size={15} />}
          label="Total nodes"
          value={String(nodeCount)}
        />
      </Stack>

      <Divider my="md" />

      <Group justify="space-between" mb={6}>
        <Text size="xs" fw={600} c="dimmed" tt="uppercase" style={{ letterSpacing: '0.03em' }}>
          Add-ons
        </Text>
        <Badge variant="light" radius="sm">
          {totalAddons}
        </Badge>
      </Group>
      <Group gap={6}>
        {bundleAddons > 0 && (
          <Badge size="sm" variant="light" color="gray" radius="sm">
            {bundleAddons} bundle
          </Badge>
        )}
        {extraAddons > 0 && (
          <Badge size="sm" variant="light" color="brand" radius="sm">
            {extraAddons} optional
          </Badge>
        )}
        {customAddons > 0 && (
          <Badge size="sm" variant="light" color="grape" radius="sm">
            {customAddons} custom
          </Badge>
        )}
        {totalAddons === 0 && (
          <Text size="xs" c="dimmed">
            none
          </Text>
        )}
      </Group>

      <Divider my="md" />

      <Group justify="space-between" mb="xs">
        <Text size="xs" fw={600} c="dimmed" tt="uppercase" style={{ letterSpacing: '0.03em' }}>
          Footprint
        </Text>
        {overBudget && (
          <Badge size="sm" color="red" variant="light" radius="sm">
            over capacity
          </Badge>
        )}
      </Group>
      <Group gap="lg" wrap="nowrap">
        <FootprintRing
          label="vCPU"
          cost={costVCPU}
          remaining={remVCPU}
          display={`${costVCPU}`}
          sub={remVCPU !== undefined ? `of ${remVCPU} free` : ''}
          over={remVCPU !== undefined && costVCPU > remVCPU}
        />
        <FootprintRing
          label="Memory"
          cost={costMemMB}
          remaining={remMemMB}
          display={`${gib(costMemMB)}G`}
          sub={remMemMB !== undefined ? `of ${gib(remMemMB)}G free` : ''}
          over={remMemMB !== undefined && costMemMB > remMemMB}
        />
        {/* Disk is admitted against its own grant, so it belongs next to the other two: a cluster
            can fit in cores and memory and still be refused on storage. */}
        <FootprintRing
          label="Disk"
          cost={costDiskGB}
          remaining={remDiskGB}
          display={`${costDiskGB}G`}
          sub={remDiskGB !== undefined ? `of ${remDiskGB}G free` : ''}
          over={remDiskGB !== undefined && costDiskGB > remDiskGB}
        />
      </Group>
    </Card>
  );
}

function SummaryRow({
  icon,
  label,
  value,
}: {
  icon: ReactNode;
  label: string;
  value: string;
}) {
  return (
    <Group justify="space-between" wrap="nowrap" gap="sm">
      <Group gap={8} wrap="nowrap" c="dimmed">
        {icon}
        <Text size="sm" c="dimmed">
          {label}
        </Text>
      </Group>
      <Text size="sm" fw={600} ta="right" truncate style={{ maxWidth: 150 }}>
        {value}
      </Text>
    </Group>
  );
}

function FootprintRing({
  label,
  cost,
  remaining,
  display,
  sub,
  over,
}: {
  label: string;
  cost: number;
  remaining?: number;
  display: string;
  sub: string;
  over: boolean;
}) {
  const pct =
    remaining && remaining > 0 ? Math.min(100, (cost / remaining) * 100) : over ? 100 : 0;
  return (
    <Group gap="xs" wrap="nowrap">
      <RingProgress
        size={64}
        thickness={6}
        roundCaps
        sections={[{ value: pct, color: over ? 'red' : 'brand' }]}
        label={
          <Text ta="center" size="xs" fw={700}>
            {display}
          </Text>
        }
      />
      <div>
        <Text size="xs" fw={600}>
          {label}
        </Text>
        <Text size="xs" c={over ? 'red' : 'dimmed'}>
          {sub}
        </Text>
      </div>
    </Group>
  );
}

function ReviewRow({ label, value }: { label: string; value: string }) {
  return (
    <Group justify="space-between" wrap="nowrap" align="flex-start">
      <Text c="dimmed" size="sm">
        {label}
      </Text>
      <Text fw={500} size="sm" ta="right" style={{ maxWidth: '70%' }}>
        {value}
      </Text>
    </Group>
  );
}

function CapacityPreview(props: {
  nodeCount: number;
  controlPlanes: number;
  workers: number;
  costVCPU: number;
  costMemMB: number;
  costDiskGB: number;
  remVCPU?: number;
  remMemMB?: number;
  remDiskGB?: number;
  overCPU: boolean;
  overMem: boolean;
  overDisk: boolean;
}) {
  const {
    costVCPU, costMemMB, costDiskGB,
    remVCPU, remMemMB, remDiskGB,
    overCPU, overMem, overDisk,
    nodeCount, controlPlanes, workers,
  } = props;
  return (
    <Stack gap="xs">
      <Text size="sm" c="dimmed">
        {nodeCount} nodes ({controlPlanes} control plane{controlPlanes > 1 ? 's' : ''} + {workers}{' '}
        worker{workers === 1 ? '' : 's'}) will consume:
      </Text>
      <Bar
        label="vCPU"
        cost={costVCPU}
        remaining={remVCPU}
        display={`${costVCPU} vCPU${remVCPU !== undefined ? ` of ${remVCPU} free` : ''}`}
        over={overCPU}
      />
      <Bar
        label="Memory"
        cost={costMemMB}
        remaining={remMemMB}
        display={`${gib(costMemMB)} GB${remMemMB !== undefined ? ` of ${gib(remMemMB)} GB free` : ''}`}
        over={overMem}
      />
      {/* Root disks - the pools' overrides included, since that's what they actually cost. */}
      <Bar
        label="Disk"
        cost={costDiskGB}
        remaining={remDiskGB}
        display={`${costDiskGB} GB${remDiskGB !== undefined ? ` of ${remDiskGB} GB free` : ''}`}
        over={overDisk}
      />
    </Stack>
  );
}

function Bar({
  label,
  cost,
  remaining,
  display,
  over,
}: {
  label: string;
  cost: number;
  remaining?: number;
  display: string;
  over: boolean;
}) {
  const value = remaining && remaining > 0 ? Math.min(100, (cost / remaining) * 100) : over ? 100 : 0;
  return (
    <div>
      <Group justify="space-between" mb={2}>
        <Text size="xs" fw={500}>
          {label}
        </Text>
        <Text size="xs" c={over ? 'red' : 'dimmed'}>
          {display}
        </Text>
      </Group>
      <Progress value={value} color={over ? 'red' : 'brand'} size="md" radius="xl" />
    </div>
  );
}
