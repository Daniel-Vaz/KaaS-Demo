// The account page (GET /auth/profile): who the signed-in user is, the groups they belong to with
// the role they hold in each, and their quota consumption per infrastructure. Read-only - every
// field here is administered elsewhere (the Admin page, or the directory).

import {
  Avatar,
  Badge,
  Card,
  Center,
  Group,
  Skeleton,
  Stack,
  Table,
  Text,
  Title,
  Tooltip,
} from '@mantine/core';
import {
  IconEye,
  IconPencil,
  IconShieldCog,
  IconUsersGroup,
} from '@tabler/icons-react';
import { useProfile } from '../lib/queries';
import { CapacityGauges } from '../components/CapacityGauges';
import { EmptyState } from '../components/EmptyState';
import { fromDirectory, directoryManaged } from '../lib/types';
import { relative } from '../lib/format';
import type { ProfileGroup, User } from '../lib/types';

// Field is one label/value row in the identity card. Values the directory never supplied (a local
// account has no display name or email) render as an explicit dash rather than an empty gap.
function Field({ label, value }: { label: string; value?: string | null }) {
  return (
    <div>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600} lh={1.6}>
        {label}
      </Text>
      {value ? (
        <Text size="sm">{value}</Text>
      ) : (
        <Text size="sm" c="dimmed">
          -
        </Text>
      )}
    </div>
  );
}

function RoleBadge({ role }: { role: ProfileGroup['role'] }) {
  const write = role === 'write';
  return (
    <Tooltip
      label={
        write
          ? 'Write - you can manage this group’s clusters'
          : 'Read - you can view this group’s clusters'
      }
      withArrow
    >
      <Badge
        variant="light"
        color={write ? 'brand' : 'gray'}
        size="sm"
        leftSection={write ? <IconPencil size={11} /> : <IconEye size={11} />}
        style={{ textTransform: 'none' }}
      >
        {write ? 'Write' : 'Read'}
      </Badge>
    </Tooltip>
  );
}

function IdentityCard({ user }: { user: User }) {
  return (
    <Card radius="md" padding="lg">
      <Group gap="md" mb="lg" wrap="nowrap">
        <Avatar radius="xl" size={56} color="brand">
          {user.username.slice(0, 2).toUpperCase()}
        </Avatar>
        <div>
          <Group gap="xs">
            <Text fw={600} size="lg" lh={1.2}>
              {user.display_name || user.username}
            </Text>
            {user.is_admin && (
              <Badge
                variant="light"
                color="orange"
                size="sm"
                leftSection={<IconShieldCog size={11} />}
                style={{ textTransform: 'none' }}
              >
                Administrator
              </Badge>
            )}
          </Group>
          <Text size="sm" c="dimmed">
            Member since {relative(user.created_at)}
          </Text>
        </div>
      </Group>

      <Stack gap="md">
        <Field label="Name" value={user.display_name} />
        <Field label="Username" value={user.username} />
        <Field label="Email" value={user.email} />
        <div>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600} lh={1.6}>
            Sign-in
          </Text>
          <Text size="sm">
            {fromDirectory(user) ? 'Active Directory account' : 'Local account'}
          </Text>
        </div>
      </Stack>
    </Card>
  );
}

// GroupsCard lists the user's memberships. A user can be in several groups at once with a different
// role in each, so the role belongs on the row rather than on the account.
function GroupsCard({ groups }: { groups: ProfileGroup[] }) {
  return (
    <Card radius="md" padding="lg">
      <Text fw={600} mb="xs">
        Your groups
      </Text>
      <Text size="xs" c="dimmed" mb="md">
        Members of a group share access to each other's clusters. Your role is per group, and set by
        an administrator - it never limits the clusters you own.
      </Text>
      {groups.length === 0 ? (
        <EmptyState
          icon={IconUsersGroup}
          title="No groups"
          description="You aren't a member of any group, so only you can see the clusters you own."
        />
      ) : (
        <Table verticalSpacing="sm" highlightOnHover>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Group</Table.Th>
              <Table.Th>Your role</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {groups.map((g) => (
              <Table.Tr key={g.id}>
                <Table.Td>
                  <Group gap="xs">
                    <Text size="sm" fw={500}>
                      {g.name}
                    </Text>
                    {directoryManaged(g) && (
                      <Badge variant="default" size="xs" radius="sm" style={{ textTransform: 'none' }}>
                        directory
                      </Badge>
                    )}
                  </Group>
                </Table.Td>
                <Table.Td>
                  <RoleBadge role={g.role} />
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
    </Card>
  );
}

export function Profile() {
  const { data: profile, isLoading } = useProfile();

  if (isLoading || !profile) {
    return (
      <Stack>
        <Skeleton height={40} width={220} radius="sm" />
        <Skeleton height={280} radius="md" />
        <Skeleton height={200} radius="md" />
      </Stack>
    );
  }

  const { user, capacity } = profile;
  const groups = profile.groups ?? [];
  const noQuota = capacity.total_vcpu === 0 && capacity.total_mem_mb === 0;

  return (
    <>
      <Title order={2} mb={4}>
        Profile
      </Title>
      <Text c="dimmed" size="sm" mb="lg">
        Your account, group access and capacity.
      </Text>

      <Stack gap="md">
        <IdentityCard user={user} />

        <Card radius="md" padding="lg">
          <Text fw={600} mb="xs">
            Quota usage
          </Text>
          <Text size="xs" c="dimmed" mb="md">
            {user.is_admin
              ? // Admins hold no stored grant - their budget on each backend is its live unallocated
                // pool, so the "total" here moves as tenants are granted capacity.
                'As an administrator you draw from each infrastructure’s unallocated pool rather than a fixed grant.'
              : 'Capacity is granted per infrastructure and can’t be moved between them - a cluster is admitted against the headroom on the infrastructure it runs on.'}
          </Text>
          {noQuota ? (
            <Center h={120}>
              <Text c="dimmed" size="sm" ta="center">
                You don't have any quota yet. Ask an administrator to grant you capacity before
                creating clusters.
              </Text>
            </Center>
          ) : (
            <CapacityGauges cap={capacity} />
          )}
        </Card>

        <GroupsCard groups={groups} />
      </Stack>
    </>
  );
}
