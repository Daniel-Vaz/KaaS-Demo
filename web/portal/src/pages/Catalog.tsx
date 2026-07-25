import { useMemo, useState } from 'react';
import {
  Title,
  Text,
  Card,
  Badge,
  Button,
  Skeleton,
  Stack,
  Group,
  Code,
  SimpleGrid,
  Tabs,
  TextInput,
  Drawer,
  Anchor,
  ThemeIcon,
  Divider,
  UnstyledButton,
  CopyButton,
  ActionIcon,
  Tooltip,
  Collapse,
  Box,
} from '@mantine/core';
import {
  IconPackage,
  IconBox,
  IconBrandDocker,
  IconCube,
  IconSearch,
  IconExternalLink,
  IconGitBranch,
  IconChevronRight,
  IconChevronDown,
  IconCopy,
  IconCheck,
  IconPackages,
  IconCode,
} from '@tabler/icons-react';
import type { Icon } from '@tabler/icons-react';
import { useCatalog } from '../lib/queries';
import { EmptyState } from '../components/EmptyState';
import { StatCard } from '../components/StatCard';
import { CustomCatalogs } from '../components/CustomCatalogs';
import { AddonValuesEditor } from '../components/AddonValuesEditor';
import type { Catalog as CatalogData, CatalogStatus, Bundle, CatalogAddon, OSImage, K8sVersion } from '../lib/types';
import classes from '../components/Catalog.module.css';

const STATUS_COLOR: Record<CatalogStatus, string> = {
  supported: 'teal',
  deprecated: 'yellow',
  eol: 'red',
};

function StatusBadge({ status }: { status: CatalogStatus }) {
  return (
    <Badge size="sm" variant="light" color={STATUS_COLOR[status] ?? 'gray'} radius="sm">
      {status}
    </Badge>
  );
}

function findAddon(catalog: CatalogData, name: string, version: string): CatalogAddon | undefined {
  return catalog.addons.find((a) => a.name === name && a.version === version);
}

// Full upgrade chain a bundle belongs to (oldest -> newest), walking `supersedes` in both
// directions. With a single bundle this is just `[bundle]`; it scales as the catalog grows.
function bundleChain(catalog: CatalogData, bundle: Bundle): Bundle[] {
  const byName = new Map(catalog.bundles.map((b) => [b.name, b]));
  const chain: Bundle[] = [bundle];
  let cur = bundle;
  while (cur.supersedes) {
    const prev = byName.get(cur.supersedes);
    if (!prev) break;
    chain.unshift(prev);
    cur = prev;
  }
  cur = bundle;
  for (;;) {
    const next = catalog.bundles.find((b) => b.supersedes === cur.name);
    if (!next) break;
    chain.push(next);
    cur = next;
  }
  return chain;
}

function matches(query: string, ...fields: string[]): boolean {
  if (!query) return true;
  return fields.some((f) => f.toLowerCase().includes(query));
}

function TabLabel({ icon: IconCmp, label, count }: { icon: Icon; label: string; count: number }) {
  return (
    <Group gap={6}>
      <IconCmp size={15} />
      <Text size="sm">{label}</Text>
      <Badge size="xs" variant="default" radius="sm" circle>
        {count}
      </Badge>
    </Group>
  );
}

function BundleCard({ bundle, onSelect }: { bundle: Bundle; onSelect: () => void }) {
  return (
    <UnstyledButton onClick={onSelect}>
      <Card radius="md" padding="lg" className={classes.clickable}>
        <Group justify="space-between" mb="sm" wrap="nowrap">
          <Group gap="xs">
            <ThemeIcon size={34} radius="md" variant="light" color="brand">
              <IconPackage size={18} />
            </ThemeIcon>
            <Text fw={700}>{bundle.name}</Text>
          </Group>
          <StatusBadge status={bundle.status} />
        </Group>

        <SimpleGrid cols={3} spacing="xs" mb="sm">
          <div>
            <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
              Kubernetes
            </Text>
            <Text size="sm" fw={500}>
              {bundle.kubernetes}
            </Text>
          </div>
          <div>
            <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
              OS
            </Text>
            <Text size="sm" fw={500}>
              {bundle.os}
            </Text>
          </div>
          <div>
            <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
              CNI
            </Text>
            <Text size="sm" fw={500}>
              {bundle.cni}
            </Text>
          </div>
        </SimpleGrid>

        <Group gap={4} mb="sm">
          {Object.entries(bundle.addons).map(([name, ver]) => (
            <Badge key={name} size="xs" variant="default" radius="sm">
              {name} {ver}
            </Badge>
          ))}
        </Group>

        <Divider mb="sm" />

        <Group justify="space-between">
          <Group gap={6}>
            <IconGitBranch size={14} color="var(--mantine-color-dimmed)" />
            <Text size="xs" c="dimmed">
              {bundle.supersedes ? `Upgrades from ${bundle.supersedes}` : 'Baseline release'}
            </Text>
          </Group>
          <IconChevronRight size={16} color="var(--mantine-color-dimmed)" />
        </Group>
      </Card>
    </UnstyledButton>
  );
}

// Group a flat map of dotted Helm value paths by their top-level segment so the
// long, repetitive keys collapse into scannable sections (e.g. everything under
// `prometheus.` sits under one `prometheus` heading, keyed by the remaining path).
function groupValues(values: Record<string, string>): [string, [string, string][]][] {
  const groups = new Map<string, [string, string][]>();
  for (const [key, val] of Object.entries(values)) {
    const dot = key.indexOf('.');
    const head = dot === -1 ? key : key.slice(0, dot);
    const leaf = dot === -1 ? key : key.slice(dot + 1);
    const rows = groups.get(head) ?? [];
    rows.push([leaf, val]);
    groups.set(head, rows);
  }
  return [...groups.entries()];
}

function AddonValues({ values }: { values: Record<string, string> }) {
  const groups = useMemo(() => groupValues(values), [values]);
  const count = Object.keys(values).length;

  return (
    <Stack gap={6} mt={6}>
      <Group gap={6}>
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
          Catalog overrides
        </Text>
        <Badge size="xs" variant="default" radius="sm" circle>
          {count}
        </Badge>
      </Group>
      <div className={classes.valuesPanel}>
        {groups.map(([head, rows]) => (
          <div key={head} className={classes.valueGroup}>
            <Text className={classes.valueGroupHead} ff="monospace">
              {head}
            </Text>
            {rows.map(([leaf, val]) => (
              <div key={leaf} className={classes.valueRow}>
                <Text component="span" ff="monospace" className={classes.valueKey}>
                  {leaf === head ? '·' : leaf}
                </Text>
                <Code className={classes.valueVal}>{val}</Code>
              </div>
            ))}
          </div>
        ))}
      </div>
    </Stack>
  );
}

function AddonRow({ addon }: { addon: CatalogAddon }) {
  const [open, setOpen] = useState(false);
  const [showValues, setShowValues] = useState(false);
  const hasValues = addon.values && Object.keys(addon.values).length > 0;

  return (
    <Box>
      <UnstyledButton onClick={() => setOpen((o) => !o)} className={classes.addonRow} p="xs" w="100%">
        <Group justify="space-between" wrap="nowrap">
          <Group gap="sm" wrap="nowrap">
            <ThemeIcon size={30} radius="md" variant="light" color={addon.type === 'cni' ? 'grape' : 'gray'}>
              <IconBox size={16} />
            </ThemeIcon>
            <div style={{ minWidth: 0 }}>
              <Text size="sm" fw={600}>
                {addon.name}
              </Text>
              <Text size="xs" c="dimmed" truncate>
                {addon.description ?? addon.repo}
              </Text>
            </div>
          </Group>
          <Group gap="sm" wrap="nowrap">
            <Code>{addon.version}</Code>
            <StatusBadge status={addon.status} />
            {open ? <IconChevronDown size={16} /> : <IconChevronRight size={16} />}
          </Group>
        </Group>
      </UnstyledButton>
      <Collapse expanded={open}>
        <Stack gap={6} pl={54} pr="xs" pb="sm" pt={4}>
          {addon.description && (
            <Text size="xs" c="dimmed">
              {addon.description}
            </Text>
          )}
          <Group gap={6}>
            <Text size="xs" c="dimmed" w={70}>
              Chart
            </Text>
            <Text size="xs">{addon.chart}</Text>
          </Group>
          <Anchor href={addon.repo} target="_blank" rel="noreferrer" size="xs" w="fit-content">
            <Group gap={4}>
              {addon.repo}
              <IconExternalLink size={12} />
            </Group>
          </Anchor>
          <Button
            size="xs"
            variant="light"
            color="gray"
            w="fit-content"
            mt={4}
            leftSection={<IconCode size={14} />}
            onClick={() => setShowValues(true)}
          >
            View default values
          </Button>
          {hasValues && <AddonValues values={addon.values!} />}
        </Stack>
      </Collapse>
      {showValues && (
        <AddonValuesEditor
          opened
          readOnly
          onClose={() => setShowValues(false)}
          addonName={addon.name}
          addonVersion={addon.version}
        />
      )}
    </Box>
  );
}

function OSImageCard({ os }: { os: OSImage }) {
  return (
    <Card radius="md" padding="lg">
      <Group justify="space-between" mb="sm">
        <Group gap="xs">
          <ThemeIcon size={34} radius="md" variant="light" color="orange">
            <IconBrandDocker size={18} />
          </ThemeIcon>
          <div>
            <Text fw={600} size="sm">
              {os.name}
            </Text>
            <Text size="xs" c="dimmed">
              {os.family} {os.release}
            </Text>
          </div>
        </Group>
        <StatusBadge status={os.status} />
      </Group>
      <Stack gap={6}>
        <Group gap={6} wrap="nowrap">
          <Text size="xs" c="dimmed" w={80}>
            Golden image
          </Text>
          <Code style={{ flex: 1 }}>{os.goldenImage}</Code>
          <CopyButton value={os.goldenImage}>
            {({ copied, copy }) => (
              <Tooltip label={copied ? 'Copied' : 'Copy'} withArrow>
                <ActionIcon variant="subtle" color={copied ? 'teal' : 'gray'} onClick={copy} size="sm">
                  {copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
                </ActionIcon>
              </Tooltip>
            )}
          </CopyButton>
        </Group>
        <Anchor href={os.baseImageURL} target="_blank" rel="noreferrer" size="xs">
          <Group gap={4}>
            <Text size="xs" truncate maw={280}>
              {os.baseImageURL}
            </Text>
            <IconExternalLink size={12} style={{ flexShrink: 0 }} />
          </Group>
        </Anchor>
      </Stack>
    </Card>
  );
}

function K8sCard({ k }: { k: K8sVersion }) {
  return (
    <Card radius="md" padding="lg">
      <Group justify="space-between">
        <Group gap="xs">
          <ThemeIcon size={34} radius="md" variant="light" color="blue">
            <IconCube size={18} />
          </ThemeIcon>
          <Text fw={700}>{k.version}</Text>
        </Group>
        <StatusBadge status={k.status} />
      </Group>
    </Card>
  );
}

function BundleDetail({ catalog, bundle, onJump }: { catalog: CatalogData; bundle: Bundle; onJump: (b: Bundle) => void }) {
  const chain = useMemo(() => bundleChain(catalog, bundle), [catalog, bundle]);
  const osImage = catalog.os.find((o) => o.name === bundle.os);

  return (
    <Stack gap="lg">
      <Group justify="space-between">
        <Group gap="xs">
          <ThemeIcon size={38} radius="md" variant="light" color="brand">
            <IconPackage size={20} />
          </ThemeIcon>
          <Title order={3}>{bundle.name}</Title>
        </Group>
        <StatusBadge status={bundle.status} />
      </Group>

      {chain.length > 1 && (
        <div>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600} mb={6}>
            Upgrade chain
          </Text>
          <Group gap={6} wrap="wrap">
            {chain.map((b, i) => (
              <Group key={b.name} gap={6} wrap="nowrap">
                {i > 0 && <IconChevronRight size={14} color="var(--mantine-color-dimmed)" />}
                <Badge
                  variant={b.name === bundle.name ? 'filled' : 'default'}
                  color={b.name === bundle.name ? 'brand' : 'gray'}
                  radius="sm"
                  className={classes.chainChip}
                  onClick={() => b.name !== bundle.name && onJump(b)}
                >
                  {b.name}
                </Badge>
              </Group>
            ))}
          </Group>
        </div>
      )}

      <SimpleGrid cols={2} spacing="md">
        <Card radius="md" padding="md" withBorder>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600} mb={4}>
            Kubernetes
          </Text>
          <Text fw={600}>{bundle.kubernetes}</Text>
        </Card>
        <Card radius="md" padding="md" withBorder>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600} mb={4}>
            OS image
          </Text>
          <Text fw={600}>{bundle.os}</Text>
          {osImage && (
            <Text size="xs" c="dimmed" truncate>
              {osImage.goldenImage}
            </Text>
          )}
        </Card>
      </SimpleGrid>

      <div>
        <Text size="xs" c="dimmed" tt="uppercase" fw={600} mb={6}>
          Add-ons ({Object.keys(bundle.addons).length})
        </Text>
        <Stack gap={4}>
          {Object.entries(bundle.addons).map(([name, ver]) => {
            const addon = findAddon(catalog, name, ver);
            return (
              <Card key={name} radius="md" padding="sm" withBorder>
                <Group justify="space-between" wrap="nowrap" align="flex-start">
                  <Group gap="sm" wrap="nowrap" align="flex-start">
                    <ThemeIcon size={28} radius="md" variant="light" color={addon?.type === 'cni' ? 'grape' : 'gray'}>
                      <IconBox size={14} />
                    </ThemeIcon>
                    <div style={{ minWidth: 0 }}>
                      <Text size="sm" fw={600}>
                        {name}
                      </Text>
                      {addon?.description && (
                        <Text size="xs" c="dimmed">
                          {addon.description}
                        </Text>
                      )}
                      {addon && (
                        <Anchor href={addon.repo} target="_blank" rel="noreferrer" size="xs" c="dimmed">
                          {addon.chart}
                        </Anchor>
                      )}
                    </div>
                  </Group>
                  <Code>{ver}</Code>
                </Group>
              </Card>
            );
          })}
        </Stack>
      </div>
    </Stack>
  );
}

export function Catalog() {
  const { data: catalog, isLoading } = useCatalog();
  const [query, setQuery] = useState('');
  const [tab, setTab] = useState<string | null>('bundles');
  const [selectedBundle, setSelectedBundle] = useState<Bundle | null>(null);

  const q = query.trim().toLowerCase();

  const bundles = useMemo(
    () => catalog?.bundles.filter((b) => matches(q, b.name, b.kubernetes, b.os, b.cni)) ?? [],
    [catalog, q],
  );
  const addons = useMemo(
    () => catalog?.addons.filter((a) => matches(q, a.name, a.version, a.description ?? '')) ?? [],
    [catalog, q],
  );
  const osImages = useMemo(
    () => catalog?.os.filter((o) => matches(q, o.name, o.family, o.release)) ?? [],
    [catalog, q],
  );
  const k8sVersions = useMemo(() => catalog?.kubernetes.filter((k) => matches(q, k.version)) ?? [], [catalog, q]);

  const cniAddons = addons.filter((a) => a.type === 'cni');
  const otherAddons = addons.filter((a) => a.type !== 'cni');

  if (isLoading || !catalog) {
    return (
      <Stack>
        <Skeleton height={40} width={200} />
        <Skeleton height={80} />
        <Skeleton height={280} />
      </Stack>
    );
  }

  return (
    <>
      <Group justify="space-between" align="flex-start" mb="xs">
        <div>
          <Title order={2}>Catalog</Title>
          <Text c="dimmed" size="sm" maw={640}>
            The authoritative, data-driven inventory of everything the platform can provision.
            Release bundles pin a coherent set and chain together via <Code>supersedes</Code> to
            form upgrade paths.
          </Text>
        </div>
        <TextInput
          placeholder="Search catalog..."
          leftSection={<IconSearch size={16} />}
          value={query}
          onChange={(e) => setQuery(e.currentTarget.value)}
          w={260}
        />
      </Group>

      <SimpleGrid cols={{ base: 2, md: 4 }} mb="lg" mt="md">
        <StatCard icon={IconPackage} label="Release bundles" value={catalog.bundles.length} color="brand" />
        <StatCard icon={IconBox} label="Add-ons" value={catalog.addons.length} color="grape" />
        <StatCard icon={IconBrandDocker} label="OS images" value={catalog.os.length} color="orange" />
        <StatCard icon={IconCube} label="Kubernetes" value={catalog.kubernetes.length} color="blue" />
      </SimpleGrid>

      <Tabs value={tab} onChange={setTab} keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="bundles">
            <TabLabel icon={IconPackage} label="Release bundles" count={bundles.length} />
          </Tabs.Tab>
          <Tabs.Tab value="addons">
            <TabLabel icon={IconBox} label="Add-ons" count={addons.length} />
          </Tabs.Tab>
          <Tabs.Tab value="images">
            <TabLabel icon={IconBrandDocker} label="OS images" count={osImages.length} />
          </Tabs.Tab>
          <Tabs.Tab value="kubernetes">
            <TabLabel icon={IconCube} label="Kubernetes" count={k8sVersions.length} />
          </Tabs.Tab>
          <Tabs.Tab value="custom" ml="auto">
            <Group gap={6}>
              <IconPackages size={15} />
              <Text size="sm">Custom catalogs</Text>
            </Group>
          </Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="bundles">
          {bundles.length === 0 ? (
            <EmptyState icon={IconPackage} title="No bundles match" description="Try a different search term." />
          ) : (
            <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="md">
              {bundles.map((b) => (
                <BundleCard key={b.name} bundle={b} onSelect={() => setSelectedBundle(b)} />
              ))}
            </SimpleGrid>
          )}
        </Tabs.Panel>

        <Tabs.Panel value="addons">
          {addons.length === 0 ? (
            <EmptyState icon={IconBox} title="No add-ons match" description="Try a different search term." />
          ) : (
            <Stack gap="lg">
              {cniAddons.length > 0 && (
                <Card radius="md" padding="lg">
                  <Text fw={600} size="sm" mb="xs">
                    CNI plugins
                  </Text>
                  <Stack gap={2}>
                    {cniAddons.map((a) => (
                      <AddonRow key={a.name + a.version} addon={a} />
                    ))}
                  </Stack>
                </Card>
              )}
              {otherAddons.length > 0 && (
                <Card radius="md" padding="lg">
                  <Text fw={600} size="sm" mb="xs">
                    Cluster add-ons
                  </Text>
                  <Stack gap={2}>
                    {otherAddons.map((a) => (
                      <AddonRow key={a.name + a.version} addon={a} />
                    ))}
                  </Stack>
                </Card>
              )}
            </Stack>
          )}
        </Tabs.Panel>

        <Tabs.Panel value="images">
          {osImages.length === 0 ? (
            <EmptyState icon={IconBrandDocker} title="No OS images match" description="Try a different search term." />
          ) : (
            <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="md">
              {osImages.map((o) => (
                <OSImageCard key={o.name} os={o} />
              ))}
            </SimpleGrid>
          )}
        </Tabs.Panel>

        <Tabs.Panel value="kubernetes">
          {k8sVersions.length === 0 ? (
            <EmptyState icon={IconCube} title="No versions match" description="Try a different search term." />
          ) : (
            <SimpleGrid cols={{ base: 2, sm: 3, lg: 4 }} spacing="md">
              {k8sVersions.map((k) => (
                <K8sCard key={k.version} k={k} />
              ))}
            </SimpleGrid>
          )}
        </Tabs.Panel>

        <Tabs.Panel value="custom">
          <CustomCatalogs />
        </Tabs.Panel>
      </Tabs>

      <Drawer
        opened={!!selectedBundle}
        onClose={() => setSelectedBundle(null)}
        title="Bundle detail"
        position="right"
        size="md"
      >
        {selectedBundle && <BundleDetail catalog={catalog} bundle={selectedBundle} onJump={setSelectedBundle} />}
      </Drawer>
    </>
  );
}
