// The Networking page's Overview tab: the cluster's north-south contract, then the apps published
// through it.
//
// This is the tab the page exists for. Every cluster this platform builds gets the same default
// ingress - one reserved address held by a default Envoy Gateway, and a wildcard DNS record pointing
// at it - so that attaching an HTTPRoute for any name under the cluster's apps domain is all a tenant
// has to do. The two cards below state that contract in the concrete terms of THIS cluster (the
// actual IP, the actual record), and the table under them answers the question it exists to serve:
// what is actually reachable from outside, and at what URL.

import { Link } from 'react-router-dom';
import {
  Card,
  Group,
  Text,
  Badge,
  Stack,
  SimpleGrid,
  Table,
  Anchor,
  Tooltip,
  CopyButton,
  ActionIcon,
  Alert,
  ThemeIcon,
} from '@mantine/core';
import {
  IconCopy,
  IconCheck,
  IconWorld,
  IconRoute,
  IconLock,
  IconLockOpen,
  IconAlertTriangle,
  IconExternalLink,
  IconCircleCheck,
  IconCircleDashed,
} from '@tabler/icons-react';
import type { NetworkOverview, ExposedApp } from '../../lib/types';
import { serviceTypeColor, formatPorts, readyColor } from '../../lib/network';
import { EmptyState } from '../EmptyState';

export function NetworkingOverview({
  overview,
  clusterId,
  onSelectRoute,
}: {
  overview: NetworkOverview;
  clusterId: string | undefined;
  onSelectRoute: (kind: string, namespace: string, name: string) => void;
}) {
  const apps = overview.exposed_apps ?? [];
  const lbServices = overview.load_balancer_services ?? [];

  return (
    <Stack gap="lg">
      {/* The add-ons the default path is made of. Deselecting one is legitimate, so this explains a
          missing gateway rather than leaving the page mysteriously empty. */}
      {!overview.addons.gateway && (
        <Alert color="yellow" icon={<IconAlertTriangle size={18} />} title="No default gateway">
          This cluster was created without the <b>envoy-gateway</b> add-on, so it has no default
          Gateway and nothing is published through the platform's standard path. Its reserved address
          is still held for it.
        </Alert>
      )}

      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="lg">
        <ContractCard
          icon={IconWorld}
          title="Gateway external IP"
          value={overview.load_balancer_ip || 'none reserved'}
          wired={overview.gateway_wired}
          wiredLabel="Gateway configured"
          pendingLabel="Not yet configured"
          hint={
            overview.load_balancer_ip
              ? 'Reserved for this cluster at admission and handed to the default Envoy Gateway by MetalLB. Every name below resolves here.'
              : 'This deployment reserves no load-balancer address for clusters.'
          }
        />
        <ContractCard
          icon={IconRoute}
          title="Wildcard DNS"
          value={overview.wildcard_record || 'no apps domain'}
          wired={overview.dns_wired}
          wiredLabel="Record published"
          pendingLabel="Not yet published"
          hint={
            overview.apps_domain
              ? `Any name under ${overview.apps_domain} resolves to this cluster's gateway - attach an HTTPRoute for it and it is reachable, with nothing else to configure.`
              : 'This deployment publishes no per-cluster DNS.'
          }
        />
      </SimpleGrid>

      {/* The default Gateway's listeners: the ports actually open, and whether HTTPS terminates. */}
      {overview.default_gateway && (
        <Card withBorder padding="md">
          <Group justify="space-between" mb="sm">
            <Text fw={600}>Default gateway listeners</Text>
            <Badge color={readyColor(overview.default_gateway.programmed)} variant="light" size="sm">
              {overview.default_gateway.programmed ? 'Programmed' : 'Pending'}
            </Badge>
          </Group>
          <Group gap="sm" wrap="wrap">
            {(overview.default_gateway.listeners ?? []).map((l) => (
              <Tooltip
                key={l.name}
                label={
                  l.tls_mode
                    ? `${l.protocol} - TLS ${l.tls_mode} for ${l.hostname || '*'} (${(l.certificate_refs ?? []).join(', ') || 'no cert'})`
                    : `${l.protocol} - plaintext, ${l.hostname || 'every hostname'}`
                }
                withArrow
              >
                <Badge
                  size="lg"
                  variant="light"
                  color={l.tls_mode ? 'green' : 'gray'}
                  leftSection={l.tls_mode ? <IconLock size={13} /> : <IconLockOpen size={13} />}
                >
                  {l.protocol} :{l.port}
                </Badge>
              </Tooltip>
            ))}
          </Group>
          {!overview.addons.cert_manager && (
            <Text c="dimmed" size="xs" mt="sm">
              Without the <b>cert-manager</b> add-on there is no HTTPS listener - routes are served
              over plain HTTP.
            </Text>
          )}
        </Card>
      )}

      {/* The headline table: everything published through a gateway, with the URL to click. */}
      <section>
        <Group justify="space-between" mb="xs" align="baseline">
          <Text fw={600}>Exposed applications</Text>
          <Text c="dimmed" size="sm">
            {apps.length} {apps.length === 1 ? 'hostname' : 'hostnames'} · {overview.route_count}{' '}
            {overview.route_count === 1 ? 'route' : 'routes'}
          </Text>
        </Group>
        {apps.length === 0 ? (
          <EmptyState
            icon={IconRoute}
            title="Nothing published yet"
            description={
              overview.apps_domain
                ? `Attach an HTTPRoute to the default gateway for any name under ${overview.apps_domain} and it shows up here - DNS and TLS are already in place.`
                : 'Attach an HTTPRoute to the default gateway and it shows up here.'
            }
          />
        ) : (
          <Card withBorder padding={0}>
            <Table.ScrollContainer minWidth={720}>
              <Table verticalSpacing="sm" highlightOnHover>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>URL</Table.Th>
                    <Table.Th>Route</Table.Th>
                    <Table.Th>Backends</Table.Th>
                    <Table.Th>DNS</Table.Th>
                    <Table.Th>Status</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {apps.map((a) => (
                    <ExposedAppRow
                      key={`${a.hostname}|${a.route.namespace}/${a.route.name}`}
                      app={a}
                      onSelectRoute={onSelectRoute}
                    />
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          </Card>
        )}
      </section>

      {/* The other way out of a cluster: a Service that holds an external address directly. Worth
          showing beside the gateway path so an operator sees every external address in one place. */}
      {lbServices.length > 0 && (
        <section>
          <Text fw={600} mb="xs">
            Load-balanced services
          </Text>
          <Card withBorder padding={0}>
            <Table.ScrollContainer minWidth={640}>
              <Table verticalSpacing="sm">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>Service</Table.Th>
                    <Table.Th>Type</Table.Th>
                    <Table.Th>External address</Table.Th>
                    <Table.Th>Ports</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {lbServices.map((s) => (
                    <Table.Tr key={`${s.namespace}/${s.name}`}>
                      <Table.Td>
                        <Text size="sm" ff="monospace" style={{ wordBreak: 'break-all' }}>
                          {s.namespace}/{s.name}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Badge color={serviceTypeColor(s.type)} variant="light" size="sm">
                          {s.type}
                        </Badge>
                      </Table.Td>
                      <Table.Td ff="monospace">{(s.external_ips ?? []).join(', ') || '-'}</Table.Td>
                      <Table.Td ff="monospace">{formatPorts(s.ports)}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          </Card>
        </section>
      )}

      {clusterId && (
        <Text c="dimmed" size="xs">
          {overview.service_count} services · {overview.gateway_count} gateways ·{' '}
          <Anchor component={Link} to={`/clusters/${clusterId}`} size="xs">
            cluster details
          </Anchor>
        </Text>
      )}
    </Stack>
  );
}

// ContractCard states one half of the platform's default ingress contract: the concrete value, and
// whether the reconciler has actually applied it yet. The pending state is not an error - a cluster
// that just went Ready is still wiring - so it reads as "not yet", not as a failure.
function ContractCard({
  icon: Icon,
  title,
  value,
  wired,
  wiredLabel,
  pendingLabel,
  hint,
}: {
  icon: typeof IconWorld;
  title: string;
  value: string;
  wired: boolean;
  wiredLabel: string;
  pendingLabel: string;
  hint: string;
}) {
  return (
    <Card withBorder padding="md">
      <Group justify="space-between" mb="xs" wrap="nowrap">
        <Group gap="xs">
          <ThemeIcon variant="light" color="gray" size="sm" radius="sm">
            <Icon size={14} />
          </ThemeIcon>
          <Text fw={600}>{title}</Text>
        </Group>
        <Badge
          color={wired ? 'green' : 'yellow'}
          variant="light"
          size="sm"
          leftSection={wired ? <IconCircleCheck size={12} /> : <IconCircleDashed size={12} />}
        >
          {wired ? wiredLabel : pendingLabel}
        </Badge>
      </Group>
      <Group gap="xs" wrap="nowrap" align="center">
        <Text ff="monospace" size="lg" style={{ wordBreak: 'break-all' }}>
          {value}
        </Text>
        <CopyButton value={value}>
          {({ copied, copy }) => (
            <Tooltip label={copied ? 'Copied' : 'Copy'} withArrow>
              <ActionIcon variant="subtle" color="gray" size="sm" onClick={copy}>
                {copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
              </ActionIcon>
            </Tooltip>
          )}
        </CopyButton>
      </Group>
      <Text c="dimmed" size="xs" mt="xs">
        {hint}
      </Text>
    </Card>
  );
}

function ExposedAppRow({
  app,
  onSelectRoute,
}: {
  app: ExposedApp;
  onSelectRoute: (kind: string, namespace: string, name: string) => void;
}) {
  const backends = (app.backends ?? []).map((b) => (b.port ? `${b.name}:${b.port}` : b.name));
  return (
    <Table.Tr>
      <Table.Td>
        <Group gap={6} wrap="nowrap">
          {app.tls ? (
            <Tooltip label="HTTPS - terminated by the gateway" withArrow>
              <IconLock size={14} />
            </Tooltip>
          ) : (
            <Tooltip label="Plain HTTP - no TLS listener matches this hostname" withArrow>
              <IconLockOpen size={14} opacity={0.5} />
            </Tooltip>
          )}
          <Anchor
            href={app.url}
            target="_blank"
            rel="noreferrer"
            size="sm"
            style={{ wordBreak: 'break-all' }}
          >
            {app.hostname}
            <IconExternalLink size={12} style={{ marginLeft: 4, verticalAlign: 'middle' }} />
          </Anchor>
        </Group>
      </Table.Td>
      <Table.Td>
        <Anchor
          component="button"
          type="button"
          size="sm"
          ff="monospace"
          onClick={() => onSelectRoute(app.route_kind, app.route.namespace, app.route.name)}
          style={{ wordBreak: 'break-all', textAlign: 'left' }}
        >
          {app.route.namespace}/{app.route.name}
        </Anchor>
      </Table.Td>
      <Table.Td>
        <Text size="sm" ff="monospace" style={{ wordBreak: 'break-all' }}>
          {backends.join(', ') || '-'}
        </Text>
      </Table.Td>
      <Table.Td>
        {/* A hostname outside the cluster's apps domain is not covered by the platform's wildcard,
            so the user owns its DNS - the usual reason a route "works" but the name doesn't resolve. */}
        {app.platform_domain ? (
          <Tooltip label="Covered by this cluster's wildcard record" withArrow>
            <Badge color="green" variant="light" size="sm">
              platform
            </Badge>
          </Tooltip>
        ) : (
          <Tooltip label="Outside the cluster's apps domain - you own this name's DNS" withArrow>
            <Badge color="gray" variant="light" size="sm">
              external
            </Badge>
          </Tooltip>
        )}
      </Table.Td>
      <Table.Td>
        <Tooltip label={app.status || (app.accepted ? 'Accepted by the gateway' : 'Pending')} withArrow>
          <Badge color={readyColor(app.accepted)} variant="light" size="sm">
            {app.accepted ? 'Ready' : 'Pending'}
          </Badge>
        </Tooltip>
      </Table.Td>
    </Table.Tr>
  );
}
