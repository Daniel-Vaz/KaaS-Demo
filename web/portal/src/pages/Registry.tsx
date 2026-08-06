import { useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router';
import {
  Group,
  Title,
  Text,
  Badge,
  Card,
  Table,
  Button,
  Anchor,
  Alert,
  Skeleton,
  Stack,
  Tooltip,
  Accordion,
  Code,
  Modal,
  CopyButton,
  ActionIcon,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import {
  IconPackages,
  IconExternalLink,
  IconAlertTriangle,
  IconKey,
  IconCopy,
  IconCheck,
  IconServer2,
  IconWorldDownload,
  IconBuildingWarehouse,
} from '@tabler/icons-react';
import { useRegistry, useRegistryRepositories } from '../lib/queries';
import { api, ApiError } from '../lib/api';
import { EmptyState } from '../components/EmptyState';
import { relative } from '../lib/format';
import { ArtifactTable } from '../components/registry/ArtifactTable';
import { PushInstructions } from '../components/registry/PushInstructions';
import type { RegistryCredential, RegistryProject, RegistryProjectKind } from '../lib/types';

// The Registry page. One central image registry (Harbor) spans every cluster, with a private project
// per cluster - so unlike Workloads/Storage/Monitoring this is a PLATFORM page with no cluster
// picker: the cluster is a column, not a filter.
//
// Everything here is already filtered server-side to what the actor may see, and each project
// carries the role they hold on it. The page renders that; it never reasons about registry
// permissions itself, which is what keeps it from disagreeing with the registry.

const KIND_LABEL: Record<RegistryProjectKind, string> = {
  cluster: 'Cluster',
  cache: 'Pull-through cache',
  library: 'Platform library',
  external: 'External',
};

const KIND_COLOR: Record<RegistryProjectKind, string> = {
  cluster: 'blue',
  cache: 'grape',
  library: 'teal',
  external: 'gray',
};

function bytes(n: number): string {
  if (n >= 1024 ** 3) return `${(n / 1024 ** 3).toFixed(1)} GiB`;
  if (n >= 1024 ** 2) return `${Math.round(n / 1024 ** 2)} MiB`;
  if (n > 0) return `${Math.round(n / 1024)} KiB`;
  return '—';
}

// authModeLabel names the identity source the registry is ACTUALLY using (the API reads it back from
// the registry rather than reporting what this deployment intends). Anything the platform has no word
// for - a Harbor behind an OIDC provider, say - is shown as-is instead of being flattened into "local
// accounts", which would be a confident wrong answer.
function authModeLabel(mode: string): string {
  if (mode === 'ldap') return 'directory sign-in';
  if (mode === 'local') return 'local accounts';
  return mode ? `${mode} sign-in` : 'unknown sign-in';
}

// ProjectRepos is the drill-down: a project's repositories, each expanding to its artifacts. Loaded
// only when a project is opened, so the page costs one request until someone asks for more.
function ProjectRepos({ project }: { project: RegistryProject }) {
  const { data, isLoading } = useRegistryRepositories(project.name, true);
  if (isLoading) return <Skeleton height={60} radius="sm" />;
  if (!data || data.length === 0) {
    return (
      <Text size="sm" c="dimmed">
        {project.kind === 'cache'
          ? 'Nothing cached yet - the first cluster to pull through this upstream will populate it.'
          : 'No images pushed yet.'}
      </Text>
    );
  }
  return (
    <Accordion variant="separated" chevronPosition="left">
      {data.map((r) => (
        <Accordion.Item key={r.full_name} value={r.full_name}>
          <Accordion.Control>
            <Group justify="space-between" pr="md" wrap="nowrap">
              <Text size="sm" fw={500}>
                {r.name}
              </Text>
              <Group gap="lg">
                <Text size="xs" c="dimmed">
                  {r.artifact_count} {r.artifact_count === 1 ? 'image' : 'images'}
                </Text>
                <Text size="xs" c="dimmed">
                  {r.pull_count} pulls
                </Text>
                <Text size="xs" c="dimmed">
                  {r.updated_at ? relative(r.updated_at) : ''}
                </Text>
              </Group>
            </Group>
          </Accordion.Control>
          <Accordion.Panel>
            <ArtifactTable project={project.name} repo={r.name} />
          </Accordion.Panel>
        </Accordion.Item>
      ))}
    </Accordion>
  );
}

// CredentialModal shows a generated registry password ONCE. The platform stores nothing, so this is
// the only time it is ever displayed - which the modal says plainly rather than implying it can be
// looked up later.
function CredentialModal({
  cred,
  onClose,
}: {
  cred: RegistryCredential | null;
  onClose: () => void;
}) {
  return (
    <Modal opened={!!cred} onClose={onClose} title="Your registry password" size="lg">
      {cred && (
        <Stack gap="sm">
          <Alert color="yellow" icon={<IconAlertTriangle size={16} />}>
            This is shown once. The platform does not store it - generate a new one if you lose it.
          </Alert>
          <Group gap="xs" wrap="nowrap">
            <Code block fz="sm" style={{ flex: 1 }}>
              {cred.password}
            </Code>
            <CopyButton value={cred.password}>
              {({ copied, copy }) => (
                <Tooltip label={copied ? 'Copied' : 'Copy'}>
                  <ActionIcon variant="subtle" color={copied ? 'teal' : 'gray'} onClick={copy}>
                    {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                  </ActionIcon>
                </Tooltip>
              )}
            </CopyButton>
          </Group>
          <PushInstructions host={cred.host} pushPrefix={`${cred.host}/<project>`} username={cred.username} />
        </Stack>
      )}
    </Modal>
  );
}

export function Registry() {
  const [params, setParams] = useSearchParams();
  const { data, isLoading } = useRegistry();
  const [cred, setCred] = useState<RegistryCredential | null>(null);
  const [rotating, setRotating] = useState(false);

  const status = data?.status;
  const projects = useMemo(() => data?.projects ?? [], [data?.projects]);
  const open = params.get('project') ?? '';

  const totals = useMemo(() => {
    const repos = projects.reduce((n, p) => n + p.repo_count, 0);
    const size = projects.reduce((n, p) => n + p.size_bytes, 0);
    const cached = projects
      .filter((p) => p.kind === 'cache')
      .reduce((n, p) => n + p.repo_count, 0);
    return { repos, size, cached };
  }, [projects]);

  const rotate = async () => {
    setRotating(true);
    try {
      setCred(await api.rotateRegistryPassword());
    } catch (e) {
      notifications.show({
        color: 'red',
        title: 'Could not generate a registry password',
        message: e instanceof ApiError ? e.message : String(e),
      });
    } finally {
      setRotating(false);
    }
  };

  if (isLoading) return <Skeleton height={320} radius="md" />;

  if (!status?.configured) {
    return (
      <>
        <Title order={2} mb="lg">
          Registry
        </Title>
        <EmptyState
          icon={IconPackages}
          title="No image registry is configured"
          description="This deployment runs without a container image registry. An operator can enable one - see the registry integration guide."
        />
      </>
    );
  }

  return (
    <>
      <Group justify="space-between" mb="lg" align="flex-start">
        <div>
          <Title order={2}>Registry</Title>
          <Text c="dimmed" size="sm">
            The platform's container image registry: a private project per cluster, plus pull-through
            caches of the public registries every cluster pulls from.
          </Text>
        </div>
        <Group gap="xs">
          {status.can_set_password && (
            <Button
              variant="default"
              leftSection={<IconKey size={16} />}
              onClick={rotate}
              loading={rotating}
            >
              Generate password
            </Button>
          )}
          {/* An anchor with an empty href is NOT an inert button: the browser resolves "" to the
              current URL, so it silently reloads the page (query string and all) and looks like a
              broken link to Harbor. A missing address therefore has to disable the control, not just
              blank its target. It is empty whenever no registry UI address is configured - which
              includes every fake-mode deployment, where there is no Harbor to open. */}
          <Tooltip
            label="No registry UI address is configured (KAAS_REGISTRY_UI_URL)"
            disabled={!!status.ui_url}
          >
            <Button
              component="a"
              href={status.ui_url || undefined}
              target="_blank"
              rel="noreferrer"
              disabled={!status.ui_url}
              data-disabled={!status.ui_url || undefined}
              onClick={(e) => {
                if (!status.ui_url) e.preventDefault();
              }}
              leftSection={<IconExternalLink size={16} />}
            >
              Open Harbor
            </Button>
          </Tooltip>
        </Group>
      </Group>

      {!status.healthy && (
        <Alert color="red" icon={<IconAlertTriangle size={16} />} mb="md" title="Registry unreachable">
          {status.message || 'The registry is not answering.'}
          {status.mirror && (
            <Text size="sm" mt={4}>
              Cluster image pulls are unaffected: containerd falls back to the upstream registries
              when a mirror does not answer.
            </Text>
          )}
        </Alert>
      )}

      <Group grow mb="lg" align="stretch">
        <Card withBorder padding="md">
          <Group gap="xs" mb={4}>
            <IconServer2 size={16} />
            <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
              Registry
            </Text>
          </Group>
          <Text fw={600}>{status.host}</Text>
          <Text size="xs" c="dimmed">
            {status.version ? `${status.version} · ` : ''}
            {authModeLabel(status.auth_mode)}
          </Text>
        </Card>
        <Card withBorder padding="md">
          <Group gap="xs" mb={4}>
            <IconBuildingWarehouse size={16} />
            <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
              Stored
            </Text>
          </Group>
          <Text fw={600}>{bytes(totals.size)}</Text>
          <Text size="xs" c="dimmed">
            {projects.length} projects · {totals.repos} repositories
          </Text>
        </Card>
        <Card withBorder padding="md">
          <Group gap="xs" mb={4}>
            <IconWorldDownload size={16} />
            <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
              Pull-through cache
            </Text>
          </Group>
          <Text fw={600}>{status.mirror ? `${totals.cached} cached` : 'Off'}</Text>
          <Text size="xs" c="dimmed">
            {status.mirror
              ? (status.upstreams ?? []).join(', ')
              : 'Clusters pull from the upstream registries directly'}
          </Text>
        </Card>
      </Group>

      {projects.length === 0 ? (
        <EmptyState
          icon={IconPackages}
          title="Nothing to show yet"
          description="A project is created for each cluster once it is ready. Create a cluster to get somewhere to push."
        />
      ) : (
        <Card withBorder padding={0}>
          <Table striped highlightOnHover verticalSpacing="sm">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Project</Table.Th>
                <Table.Th>Kind</Table.Th>
                <Table.Th>Cluster</Table.Th>
                <Table.Th>Your access</Table.Th>
                <Table.Th>Repositories</Table.Th>
                <Table.Th>Size</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {projects.map((p) => (
                <Table.Tr
                  key={p.name}
                  style={{ cursor: 'pointer' }}
                  onClick={() =>
                    setParams(
                      (prev) => {
                        const next = new URLSearchParams(prev);
                        if (open === p.name) next.delete('project');
                        else next.set('project', p.name);
                        return next;
                      },
                      { replace: true },
                    )
                  }
                >
                  <Table.Td>
                    <Text size="sm" fw={500}>
                      {p.name}
                    </Text>
                    {p.upstream && (
                      <Text size="xs" c="dimmed">
                        proxies {p.upstream}
                      </Text>
                    )}
                  </Table.Td>
                  <Table.Td>
                    <Badge color={KIND_COLOR[p.kind]} variant="light" size="sm">
                      {KIND_LABEL[p.kind]}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    {p.cluster_id ? (
                      <Anchor component={Link} to={`/clusters/${p.cluster_id}`} size="sm">
                        {p.cluster_name}
                      </Anchor>
                    ) : (
                      <Text size="sm" c="dimmed">
                        —
                      </Text>
                    )}
                  </Table.Td>
                  <Table.Td>
                    {/* The role comes from the platform's own model, so what this says and what the
                        registry permits are two readings of one function. */}
                    <Text size="sm" c={p.role === 'guest' ? 'dimmed' : undefined}>
                      {p.role === 'projectAdmin'
                        ? 'Full'
                        : p.role === 'developer'
                          ? 'Pull + push'
                          : 'Pull only'}
                    </Text>
                  </Table.Td>
                  <Table.Td>{p.repo_count}</Table.Td>
                  <Table.Td>{bytes(p.size_bytes)}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Card>
      )}

      {open && (
        <Card withBorder padding="md" mt="md">
          <Group justify="space-between" mb="sm">
            <Title order={4}>{open}</Title>
            {projects.find((p) => p.name === open)?.kind === 'cluster' && (
              <Text size="xs" c="dimmed">
                push to <Code fz="xs">{`${status.host}/${open}`}</Code>
              </Text>
            )}
          </Group>
          <ProjectRepos project={projects.find((p) => p.name === open) as RegistryProject} />
        </Card>
      )}

      <CredentialModal cred={cred} onClose={() => setCred(null)} />
    </>
  );
}
