import { useMemo, useState } from 'react';
import {
  Card,
  Group,
  Text,
  Badge,
  Button,
  Stack,
  SimpleGrid,
  ThemeIcon,
  Drawer,
  Modal,
  TextInput,
  Textarea,
  ActionIcon,
  Tooltip,
  Alert,
  Divider,
  Code,
  Menu,
  useComputedColorScheme,
} from '@mantine/core';
import {
  IconBox,
  IconPlus,
  IconTrash,
  IconPencil,
  IconDots,
  IconUser,
  IconEye,
  IconDownload,
  IconInfoCircle,
  IconPackages,
} from '@tabler/icons-react';
import CodeMirror from '@uiw/react-codemirror';
import { yaml } from '@codemirror/lang-yaml';
import { oneDark } from '@codemirror/theme-one-dark';
import { useCustomCatalogs, useCustomCatalogMutations } from '../lib/queries';
import { api, ApiError } from '../lib/api';
import { EmptyState } from './EmptyState';
import type { CustomAddon, CustomCatalogView } from '../lib/types';

// CustomCatalogs is the "Custom catalogs" tab under the Catalog page: users manage their own
// collections of Helm-chart add-ons (shared through the group model like clusters) and install them
// on clusters. Editors (owner / write-role group-mate / admin) can create and modify; read-role
// group-mates see a read-only view.
export function CustomCatalogs() {
  const { data: catalogs, isLoading } = useCustomCatalogs();
  const { create } = useCustomCatalogMutations();
  const [openId, setOpenId] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState('');

  const selected = useMemo(() => catalogs?.find((c) => c.id === openId) ?? null, [catalogs, openId]);

  return (
    <>
      <Group justify="space-between" mb="md">
        <Text c="dimmed" size="sm" maw={620}>
          Define your own Helm-chart add-ons and install them on clusters. Catalogs are owned by you
          and shared with your groups - group-mates with the <b>Write</b> role can edit them, others
          can view. Deleting a catalog never touches clusters that already installed its add-ons.
        </Text>
        <Button leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
          New catalog
        </Button>
      </Group>

      {isLoading ? (
        <Text c="dimmed" size="sm">
          Loading catalogs…
        </Text>
      ) : !catalogs || catalogs.length === 0 ? (
        <EmptyState
          icon={IconPackages}
          title="No custom catalogs yet"
          description="Create a catalog to curate your own Helm-chart add-ons for cluster provisioning."
          action={
            <Button mt="sm" variant="light" leftSection={<IconPlus size={16} />} onClick={() => setCreating(true)}>
              New catalog
            </Button>
          }
        />
      ) : (
        <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="md">
          {catalogs.map((cc) => (
            <CatalogCard key={cc.id} catalog={cc} onOpen={() => setOpenId(cc.id)} />
          ))}
        </SimpleGrid>
      )}

      <Drawer
        opened={!!selected}
        onClose={() => setOpenId(null)}
        position="right"
        size="lg"
        title="Custom catalog"
      >
        {selected && <CatalogDetail catalog={selected} />}
      </Drawer>

      <CreateCatalogModal
        opened={creating}
        name={newName}
        setName={setNewName}
        loading={create.isPending}
        onClose={() => setCreating(false)}
        onCreate={() =>
          create.mutate(newName.trim(), {
            onSuccess: () => {
              setCreating(false);
              setNewName('');
            },
          })
        }
      />
    </>
  );
}

function CatalogCard({ catalog, onOpen }: { catalog: CustomCatalogView; onOpen: () => void }) {
  const count = catalog.addons?.length ?? 0;
  return (
    <Card radius="md" padding="lg" withBorder style={{ cursor: 'pointer' }} onClick={onOpen}>
      <Group justify="space-between" mb="sm" wrap="nowrap">
        <Group gap="xs" wrap="nowrap">
          <ThemeIcon size={34} radius="md" variant="light" color="brand">
            <IconPackages size={18} />
          </ThemeIcon>
          <Text fw={700} truncate>
            {catalog.name}
          </Text>
        </Group>
        <Badge
          size="sm"
          variant="light"
          color={catalog.access === 'edit' ? 'teal' : 'gray'}
          leftSection={catalog.access === 'edit' ? <IconPencil size={10} /> : <IconEye size={10} />}
        >
          {catalog.access === 'edit' ? 'can edit' : 'view only'}
        </Badge>
      </Group>
      <Group gap="xs" mb="sm">
        <Badge size="sm" variant="default" radius="sm" leftSection={<IconBox size={11} />}>
          {count} add-on{count === 1 ? '' : 's'}
        </Badge>
      </Group>
      <Divider mb="sm" />
      <Group gap={6}>
        <IconUser size={13} color="var(--mantine-color-dimmed)" />
        <Text size="xs" c="dimmed">
          {catalog.owner_username}
        </Text>
      </Group>
    </Card>
  );
}

function CatalogDetail({ catalog }: { catalog: CustomCatalogView }) {
  const { remove, removeAddon } = useCustomCatalogMutations();
  const canEdit = catalog.access === 'edit';
  const [editingAddon, setEditingAddon] = useState<CustomAddon | null>(null);
  const [addingAddon, setAddingAddon] = useState(false);
  const addons = catalog.addons ?? [];

  return (
    <Stack>
      <Group justify="space-between">
        <div>
          <Group gap="xs">
            <Text fw={700} size="lg">
              {catalog.name}
            </Text>
            <Badge size="sm" variant="light" color={canEdit ? 'teal' : 'gray'}>
              {canEdit ? 'can edit' : 'view only'}
            </Badge>
          </Group>
          <Text size="xs" c="dimmed">
            owned by {catalog.owner_username}
          </Text>
        </div>
        {canEdit && (
          <Menu position="bottom-end" withArrow>
            <Menu.Target>
              <ActionIcon variant="subtle" color="gray">
                <IconDots size={18} />
              </ActionIcon>
            </Menu.Target>
            <Menu.Dropdown>
              <Menu.Item
                color="red"
                leftSection={<IconTrash size={14} />}
                onClick={() => {
                  if (confirm(`Delete catalog "${catalog.name}"? Clusters keep their installed add-ons.`)) {
                    remove.mutate(catalog.id);
                  }
                }}
              >
                Delete catalog
              </Menu.Item>
            </Menu.Dropdown>
          </Menu>
        )}
      </Group>

      {!canEdit && (
        <Alert variant="light" color="gray" icon={<IconEye size={16} />}>
          Read-only access - editing this catalog is available to its owner, group members with the{' '}
          <b>Write</b> role, and admins.
        </Alert>
      )}

      <Group justify="space-between">
        <Text fw={600} size="sm">
          Add-ons ({addons.length})
        </Text>
        {canEdit && (
          <Button size="xs" variant="light" leftSection={<IconPlus size={14} />} onClick={() => setAddingAddon(true)}>
            Add add-on
          </Button>
        )}
      </Group>

      {addons.length === 0 ? (
        <Text size="sm" c="dimmed">
          No add-ons yet.
        </Text>
      ) : (
        <Stack gap="xs">
          {addons.map((ad) => (
            <Card key={ad.name} radius="md" padding="sm" withBorder>
              <Group justify="space-between" wrap="nowrap" align="flex-start">
                <div style={{ minWidth: 0 }}>
                  <Group gap={6}>
                    <Text fw={600} size="sm">
                      {ad.name}
                    </Text>
                    <Code>{ad.version}</Code>
                    {ad.values && ad.values.trim() && (
                      <Badge size="xs" variant="light" color="yellow">
                        custom values
                      </Badge>
                    )}
                  </Group>
                  {ad.description && (
                    <Text size="xs" c="dimmed">
                      {ad.description}
                    </Text>
                  )}
                  <Text size="xs" c="dimmed" truncate>
                    {ad.chart}
                    {ad.repo ? ` · ${ad.repo}` : ''}
                  </Text>
                </div>
                {canEdit && (
                  <Group gap={4} wrap="nowrap">
                    <Tooltip label="Edit" withArrow>
                      <ActionIcon variant="subtle" color="gray" onClick={() => setEditingAddon(ad)}>
                        <IconPencil size={15} />
                      </ActionIcon>
                    </Tooltip>
                    <Tooltip label="Remove" withArrow>
                      <ActionIcon
                        variant="subtle"
                        color="red"
                        onClick={() => {
                          if (confirm(`Remove add-on "${ad.name}"?`)) {
                            removeAddon.mutate({ id: catalog.id, name: ad.name });
                          }
                        }}
                      >
                        <IconTrash size={15} />
                      </ActionIcon>
                    </Tooltip>
                  </Group>
                )}
              </Group>
            </Card>
          ))}
        </Stack>
      )}

      {(addingAddon || editingAddon) && (
        <AddonFormModal
          opened
          catalogId={catalog.id}
          existing={editingAddon ?? undefined}
          onClose={() => {
            setAddingAddon(false);
            setEditingAddon(null);
          }}
        />
      )}
    </Stack>
  );
}

function CreateCatalogModal({
  opened,
  name,
  setName,
  loading,
  onClose,
  onCreate,
}: {
  opened: boolean;
  name: string;
  setName: (v: string) => void;
  loading: boolean;
  onClose: () => void;
  onCreate: () => void;
}) {
  return (
    <Modal opened={opened} onClose={onClose} title="New custom catalog" size="sm">
      <Stack>
        <TextInput
          label="Catalog name"
          placeholder="team-charts"
          data-autofocus
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && name.trim().length >= 2) onCreate();
          }}
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            Cancel
          </Button>
          <Button loading={loading} disabled={name.trim().length < 2} onClick={onCreate}>
            Create
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}

// AddonFormModal creates or edits one custom add-on: chart coordinates + a values editor seeded from
// the chart's fetched defaults. On an existing add-on the name is fixed (rename = remove + add).
function AddonFormModal({
  opened,
  catalogId,
  existing,
  onClose,
}: {
  opened: boolean;
  catalogId: string;
  existing?: CustomAddon;
  onClose: () => void;
}) {
  const { addAddon, updateAddon } = useCustomCatalogMutations();
  const dark = useComputedColorScheme('dark') === 'dark';
  const [name, setName] = useState(existing?.name ?? '');
  const [description, setDescription] = useState(existing?.description ?? '');
  const [repo, setRepo] = useState(existing?.repo ?? '');
  const [chart, setChart] = useState(existing?.chart ?? '');
  const [version, setVersion] = useState(existing?.version ?? '');
  const [values, setValues] = useState(existing?.values ?? '');
  const [fetching, setFetching] = useState(false);
  const [fetchErr, setFetchErr] = useState<string | null>(null);

  const isEdit = !!existing;
  const oci = chart.trim().startsWith('oci://');
  const pending = addAddon.isPending || updateAddon.isPending;
  const valid = name.trim().length >= 1 && chart.trim() && version.trim() && (oci || repo.trim());

  const fetchDefaults = async () => {
    setFetching(true);
    setFetchErr(null);
    try {
      const v = await api.fetchChartValues(repo.trim(), chart.trim(), version.trim());
      setValues(v);
    } catch (err) {
      setFetchErr(err instanceof ApiError ? err.message : 'Could not fetch values');
    } finally {
      setFetching(false);
    }
  };

  const submit = () => {
    const addon: CustomAddon = {
      name: name.trim(),
      description: description.trim() || undefined,
      repo: oci ? undefined : repo.trim(),
      chart: chart.trim(),
      version: version.trim(),
      values: values.trim() || undefined,
    };
    const onSuccess = () => onClose();
    if (isEdit) updateAddon.mutate({ id: catalogId, name: existing!.name, addon }, { onSuccess });
    else addAddon.mutate({ id: catalogId, addon }, { onSuccess });
  };

  return (
    <Modal opened={opened} onClose={onClose} size="xl" title={isEdit ? `Edit add-on - ${existing!.name}` : 'New add-on'}>
      <Stack>
        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          <TextInput
            label="Add-on name"
            description="Lowercase DNS label - also the Helm release name"
            placeholder="podinfo"
            withAsterisk
            disabled={isEdit}
            value={name}
            onChange={(e) => setName(e.currentTarget.value)}
          />
          <TextInput
            label="Version"
            placeholder="6.5.0"
            withAsterisk
            value={version}
            onChange={(e) => setVersion(e.currentTarget.value)}
          />
        </SimpleGrid>
        <TextInput
          label="Chart"
          description="Chart name, or a full oci:// reference"
          placeholder="podinfo  (or oci://ghcr.io/stefanprodan/charts/podinfo)"
          withAsterisk
          value={chart}
          onChange={(e) => setChart(e.currentTarget.value)}
        />
        <TextInput
          label="Repo URL"
          description={oci ? 'Not needed - an oci:// chart carries its registry in the ref' : 'Classic HTTP Helm chart repository'}
          placeholder="https://stefanprodan.github.io/podinfo"
          disabled={oci}
          value={repo}
          onChange={(e) => setRepo(e.currentTarget.value)}
        />
        <Textarea
          label="Description"
          placeholder="What this add-on does"
          autosize
          minRows={1}
          value={description}
          onChange={(e) => setDescription(e.currentTarget.value)}
        />

        <Divider label="Helm values" labelPosition="left" />
        <Alert variant="light" color="blue" icon={<IconInfoCircle size={16} />} p="xs">
          Fetch the chart's default values to start from a real baseline, then edit only what you need.
          Leave empty to install with the chart's built-in defaults.
        </Alert>
        <Group>
          <Button
            size="xs"
            variant="light"
            leftSection={<IconDownload size={14} />}
            loading={fetching}
            disabled={!chart.trim() || !version.trim() || (!oci && !repo.trim())}
            onClick={fetchDefaults}
          >
            Fetch default values
          </Button>
          {fetchErr && (
            <Text size="xs" c="red">
              {fetchErr}
            </Text>
          )}
        </Group>
        <CodeMirror
          value={values}
          height="34vh"
          theme={dark ? oneDark : undefined}
          extensions={[yaml()]}
          onChange={setValues}
          basicSetup={{ lineNumbers: true, foldGutter: true, highlightActiveLine: true }}
        />

        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            Cancel
          </Button>
          <Button loading={pending} disabled={!valid} onClick={submit}>
            {isEdit ? 'Save' : 'Add add-on'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
