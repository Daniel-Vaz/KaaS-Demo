import { useState } from 'react';
import { Navigate } from 'react-router-dom';
import {
  Title,
  Text,
  Card,
  Table,
  Badge,
  Group,
  NumberInput,
  TextInput,
  Button,
  ActionIcon,
  Tooltip,
  Skeleton,
  SimpleGrid,
  Stack,
  Tabs,
  Paper,
  ThemeIcon,
  Avatar,
  SegmentedControl,
  Divider,
  Select,
  Progress,
} from '@mantine/core';
import { modals } from '@mantine/modals';
import {
  IconTrash,
  IconDeviceFloppy,
  IconShieldCheck,
  IconUsers,
  IconCpu,
  IconDatabase,
  IconCloud,
  IconPlus,
  IconUsersGroup,
  IconEye,
  IconPencil,
  IconX,
  IconUserPlus,
  IconLock,
  IconAddressBook,
  IconDeviceSdCard,
} from '@tabler/icons-react';
import type { Icon } from '@tabler/icons-react';
import {
  useUsers,
  useUpdateUser,
  useDeleteUser,
  useGroups,
  useCreateGroup,
  useRenameGroup,
  useDeleteGroup,
} from '../lib/queries';
import { useAuth } from '../lib/auth';
import { StatCard } from '../components/StatCard';
import { providerLabel } from '../lib/cluster';
import { gib, relative, pct } from '../lib/format';
import { directoryLocked, directoryManaged, fromDirectory } from '../lib/types';
import type {
  UserView,
  GroupView,
  GroupRole,
  GroupMembership,
  ResourceQuota,
} from '../lib/types';

// Admin is the platform-operator console. Two tabs: Users - manages tenant accounts and distributes
// each infrastructure's capacity - and Groups, where teams are created and their membership (who's
// in, and each member's read/write role) is managed directly.
//
// Quota is granted PER INFRASTRUCTURE, and the conserved-pool invariant (sum of all non-admin
// grants on a backend <= that backend's ceiling) is enforced server-side, per backend: capacity is
// not fungible, so KVM headroom cannot fund a vSphere cluster. The admin itself holds no fixed
// slice on any of them - its budget on each is whatever's left unallocated there, computed live, so
// granting a tenant never requires first shrinking the admin. Admin-only - non-admins are
// redirected away.
export function Admin() {
  const { user } = useAuth();
  const isAdmin = !!user?.is_admin;
  const { data, isLoading } = useUsers(isAdmin);
  const { data: groups, isLoading: groupsLoading } = useGroups(isAdmin);
  const sharedQuota = !!data?.shared_quota;

  if (user && !user.is_admin) return <Navigate to="/" replace />;

  return (
    <>
      <Title order={2}>Administration</Title>
      {sharedQuota ? (
        <Text c="dimmed" size="sm" mb="lg">
          Per-user quota is <b>off</b> for this deployment - every account automatically draws from
          each infrastructure's <b>full ceiling</b>, so there are no grants to hand out. The pool is
          shared first-come-first-served and can't be physically oversubscribed; the tables below show
          how much of each backend's capacity every account is currently consuming. You still organize
          teams into groups: a <b>Write</b> role lets a member manage that group's clusters, while{' '}
          <b>Read</b> is view-only.
        </Text>
      ) : (
        <Text c="dimmed" size="sm" mb="lg">
          Manage accounts, grant capacity on each infrastructure, and organize teams into groups. New
          accounts start with no quota anywhere and no groups. Quota is granted <b>per infrastructure</b>
          {' '}and can't be spent on another - a tenant needs capacity on vSphere to create a vSphere
          cluster, however much room they have on the KVM host. A user can belong to several groups at
          once with an independent role in each; a <b>Write</b> role lets them manage that group's
          clusters, while <b>Read</b> is view-only.
        </Text>
      )}

      <Tabs defaultValue="users" keepMounted={false}>
        <Tabs.List mb="lg">
          <Tabs.Tab value="users" leftSection={<IconUsers size={16} />}>
            Users
          </Tabs.Tab>
          <Tabs.Tab value="groups" leftSection={<IconUsersGroup size={16} />}>
            Groups
          </Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel value="users">
          <UsersTab data={data} isLoading={isLoading} groups={groups ?? []} selfId={user?.id} />
        </Tabs.Panel>

        <Tabs.Panel value="groups">
          <GroupsTab
            groups={groups ?? []}
            users={data?.users ?? []}
            isLoading={groupsLoading || isLoading}
          />
        </Tabs.Panel>
      </Tabs>
    </>
  );
}

// AllocationBar shows one backend's conserved pool as a single stacked bar: capacity granted to
// tenants, capacity the admins' own clusters are already consuming, and the free-to-grant remainder.
// Both spoken-for segments matter - an admin's running clusters draw from the same pool as a grant,
// so a bar that showed only tenant grants would report headroom the backend can't actually honour.
function AllocationBar({
  icon: IconCmp,
  label,
  allocated,
  adminUsed,
  total,
  fmt,
}: {
  icon: Icon;
  label: string;
  allocated: number;
  adminUsed: number;
  total: number;
  fmt: (n: number) => string;
}) {
  const free = Math.max(0, total - allocated - adminUsed);
  const spoken = allocated + adminUsed;
  const hot = pct(spoken, total) >= 90;
  return (
    <Paper p="md" radius="md">
      <Group justify="space-between" mb={6}>
        <Group gap={6}>
          <IconCmp size={16} stroke={1.6} />
          <Text size="sm" fw={600}>
            {label}
          </Text>
        </Group>
        <Text size="sm" c="dimmed">
          {fmt(spoken)} / {fmt(total)} · {pct(spoken, total)}%
        </Text>
      </Group>
      <Progress.Root size="lg" radius="xl">
        <Tooltip label={`Granted to tenants: ${fmt(allocated)}`}>
          <Progress.Section value={pct(allocated, total)} color={hot ? 'red' : 'brand'} />
        </Tooltip>
        <Tooltip label={`Used by admins' own clusters: ${fmt(adminUsed)}`}>
          <Progress.Section value={pct(adminUsed, total)} color="orange" />
        </Tooltip>
      </Progress.Root>
      <Group gap="md" mt={8}>
        <LegendDot color="var(--mantine-color-brand-6)" text={`${fmt(allocated)} granted`} />
        <LegendDot color="var(--mantine-color-orange-6)" text={`${fmt(adminUsed)} admin`} />
        <LegendDot color="var(--mantine-color-dimmed)" text={`${fmt(free)} free`} />
      </Group>
    </Paper>
  );
}

// SharedPoolBar is AllocationBar's counterpart for shared-quota mode: one backend's ceiling shown as
// how much is currently IN USE across every account (there are no grants to distinguish), and the
// free remainder. Anyone can draw from that free capacity until it runs out.
function SharedPoolBar({
  icon: IconCmp,
  label,
  used,
  total,
  fmt,
}: {
  icon: Icon;
  label: string;
  used: number;
  total: number;
  fmt: (n: number) => string;
}) {
  const free = Math.max(0, total - used);
  const hot = pct(used, total) >= 90;
  return (
    <Paper p="md" radius="md">
      <Group justify="space-between" mb={6}>
        <Group gap={6}>
          <IconCmp size={16} stroke={1.6} />
          <Text size="sm" fw={600}>
            {label}
          </Text>
        </Group>
        <Text size="sm" c="dimmed">
          {fmt(used)} / {fmt(total)} · {pct(used, total)}%
        </Text>
      </Group>
      <Progress.Root size="lg" radius="xl">
        <Tooltip label={`In use across all accounts: ${fmt(used)}`}>
          <Progress.Section value={pct(used, total)} color={hot ? 'red' : 'brand'} />
        </Tooltip>
      </Progress.Root>
      <Group gap="md" mt={8}>
        <LegendDot color="var(--mantine-color-brand-6)" text={`${fmt(used)} in use`} />
        <LegendDot color="var(--mantine-color-dimmed)" text={`${fmt(free)} free`} />
      </Group>
    </Paper>
  );
}

function LegendDot({ color, text }: { color: string; text: string }) {
  return (
    <Group gap={5} wrap="nowrap">
      <span style={{ width: 8, height: 8, borderRadius: 8, background: color, flex: 'none' }} />
      <Text size="xs" c="dimmed">
        {text}
      </Text>
    </Group>
  );
}

function UsersTab({
  data,
  isLoading,
  groups,
  selfId,
}: {
  data: ReturnType<typeof useUsers>['data'];
  isLoading: boolean;
  groups: GroupView[];
  selfId?: string;
}) {
  // Each infrastructure is its own conserved pool: capacity can't move between the KVM host and
  // vCenter, so a grant is made against ONE backend's ceiling and checked against that alone.
  // There is no meaningful platform-wide pool to show - showing one would invite the operator to
  // grant capacity that no single backend can actually honour.
  const allocation = data?.allocation ?? [];
  const providers = allocation.map((p) => p.provider);
  const sharedQuota = !!data?.shared_quota;
  const users = data?.users ?? [];

  // In shared-quota mode there are no grants; what matters is how much of each backend's ceiling is
  // in use across every account combined. Sum every user's usage on the provider.
  const consumedOn = (provider: string): ResourceQuota =>
    users.reduce(
      (acc, u) => {
        const use = u.usage?.[provider] ?? { vcpu: 0, mem_mb: 0, disk_gb: 0 };
        return {
          vcpu: acc.vcpu + use.vcpu,
          mem_mb: acc.mem_mb + use.mem_mb,
          disk_gb: acc.disk_gb + use.disk_gb,
        };
      },
      { vcpu: 0, mem_mb: 0, disk_gb: 0 },
    );

  return (
    <>
      <div style={{ maxWidth: 320 }}>
        <StatCard icon={IconUsers} label="Accounts" value={data ? String(data.users.length) : '-'} />
      </div>

      <Stack gap="md" mt="md" mb="lg">
        {allocation.map((p) => {
          const used = sharedQuota ? consumedOn(p.provider) : { vcpu: 0, mem_mb: 0, disk_gb: 0 };
          return (
            <Paper key={p.provider} p="md" radius="md" withBorder>
              <Group justify="space-between" mb="sm" wrap="wrap" gap="xs">
                <Group gap={8}>
                  <ThemeIcon
                    size={32}
                    radius="md"
                    variant="light"
                    color={p.provider === 'vsphere' ? 'blue' : 'brand'}
                  >
                    {p.provider === 'vsphere' ? (
                      <IconCloud size={17} stroke={1.6} />
                    ) : (
                      <IconCpu size={17} stroke={1.6} />
                    )}
                  </ThemeIcon>
                  <Text fw={700}>
                    {providerLabel(p.provider)} - {sharedQuota ? 'in use' : 'allocated'}
                  </Text>
                </Group>
                <Text size="xs" c="dimmed">
                  {sharedQuota
                    ? `${Math.max(0, p.total_vcpu - used.vcpu)} vCPU free - shared across all accounts`
                    : `${Math.max(0, p.total_vcpu - p.allocated_vcpu - p.admin_used_vcpu)} vCPU free to grant - after tenant grants and the admins' own clusters`}
                </Text>
              </Group>
              <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="md">
                {sharedQuota ? (
                  <>
                    <SharedPoolBar
                      icon={IconCpu}
                      label="vCPU"
                      used={used.vcpu}
                      total={p.total_vcpu}
                      fmt={(n) => String(n)}
                    />
                    <SharedPoolBar
                      icon={IconDatabase}
                      label="Memory"
                      used={used.mem_mb}
                      total={p.total_mem_mb}
                      fmt={(n) => `${gib(n)} GiB`}
                    />
                    <SharedPoolBar
                      icon={IconDeviceSdCard}
                      label="Disk"
                      used={used.disk_gb}
                      total={p.total_disk_gb}
                      fmt={(n) => `${n} GB`}
                    />
                  </>
                ) : (
                  <>
                    <AllocationBar
                      icon={IconCpu}
                      label="vCPU"
                      allocated={p.allocated_vcpu}
                      adminUsed={p.admin_used_vcpu}
                      total={p.total_vcpu}
                      fmt={(n) => String(n)}
                    />
                    <AllocationBar
                      icon={IconDatabase}
                      label="Memory"
                      allocated={p.allocated_mem_mb}
                      adminUsed={p.admin_used_mem_mb}
                      total={p.total_mem_mb}
                      fmt={(n) => `${gib(n)} GiB`}
                    />
                    {/* Storage is a conserved pool per backend like the other two: a pool's
                        root-disk override and a node's extra disks spend it. */}
                    <AllocationBar
                      icon={IconDeviceSdCard}
                      label="Disk"
                      allocated={p.allocated_disk_gb}
                      adminUsed={p.admin_used_disk_gb}
                      total={p.total_disk_gb}
                      fmt={(n) => `${n} GB`}
                    />
                  </>
                )}
              </SimpleGrid>
            </Paper>
          );
        })}
      </Stack>

      <Card padding={0} radius="md">
        {isLoading ? (
          <div style={{ padding: 16 }}>
            {[0, 1, 2].map((i) => (
              <Skeleton key={i} height={44} mb={8} radius="sm" />
            ))}
          </div>
        ) : (
          <Table.ScrollContainer minWidth={1040}>
            <Table verticalSpacing="sm">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Account</Table.Th>
                  <Table.Th>Groups &amp; roles</Table.Th>
                  <Table.Th>Clusters</Table.Th>
                  <Table.Th>
                    {sharedQuota
                      ? 'Consumption per infrastructure'
                      : 'Quota per infrastructure (granted · in use)'}
                  </Table.Th>
                  <Table.Th>Created</Table.Th>
                  <Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {(data?.users ?? []).map((u) => (
                  <UserRow
                    key={u.id}
                    u={u}
                    selfId={selfId}
                    groups={groups}
                    providers={providers}
                    sharedQuota={sharedQuota}
                  />
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
      </Card>
    </>
  );
}

// GroupsTab is the home for team management: create/rename/delete groups, and manage each group's
// membership and per-member role in place (the "who is in this group" view). Membership edits are
// sent per-user via PATCH /users/{id} with the user's FULL membership set (the backend replaces it
// wholesale), but the UI here is organized by group, which reads far more naturally for an operator.
function GroupsTab({
  groups,
  users,
  isLoading,
}: {
  groups: GroupView[];
  users: UserView[];
  isLoading: boolean;
}) {
  const create = useCreateGroup();
  const [newName, setNewName] = useState('');

  const submit = () => {
    const name = newName.trim();
    if (name.length < 2) return;
    create.mutate(name, { onSuccess: () => setNewName('') });
  };

  return (
    <Stack gap="lg">
      <Paper p="md" radius="md">
        <Group justify="space-between" gap="md" wrap="wrap">
          <Group gap="sm" wrap="nowrap">
            <ThemeIcon size={38} radius="md" variant="light" color="brand">
              <IconUsersGroup size={20} stroke={1.6} />
            </ThemeIcon>
            <div>
              <Text fw={600}>Groups</Text>
              <Text size="xs" c="dimmed">
                Members share access to each other's clusters. <b>Write</b> grants full management
                (scale, upgrade, delete, admin kubeconfig, shell); <b>Read</b> is view-only, with a
                read-only kubeconfig and shell.
              </Text>
            </div>
          </Group>
          <Group gap="xs" wrap="nowrap">
            <TextInput
              placeholder="New group name"
              value={newName}
              onChange={(e) => setNewName(e.currentTarget.value)}
              onKeyDown={(e) => e.key === 'Enter' && submit()}
              size="sm"
              w={200}
            />
            <Button
              size="sm"
              leftSection={<IconPlus size={16} />}
              disabled={newName.trim().length < 2}
              loading={create.isPending}
              onClick={submit}
            >
              Add group
            </Button>
          </Group>
        </Group>
      </Paper>

      {isLoading ? (
        <Stack gap="md">
          {[0, 1].map((i) => (
            <Skeleton key={i} height={140} radius="md" />
          ))}
        </Stack>
      ) : groups.length === 0 ? (
        <Card radius="md" padding="xl">
          <Stack align="center" gap={6}>
            <ThemeIcon size={44} radius="xl" variant="light" color="gray">
              <IconUsersGroup size={24} stroke={1.5} />
            </ThemeIcon>
            <Text fw={600}>No groups yet</Text>
            <Text size="sm" c="dimmed" ta="center" maw={360}>
              Create a group above, then add users to it and set each member's role.
            </Text>
          </Stack>
        </Card>
      ) : (
        <SimpleGrid cols={{ base: 1, lg: 2 }} spacing="md">
          {groups.map((g) => (
            <GroupCard key={g.id} group={g} users={users} />
          ))}
        </SimpleGrid>
      )}
    </Stack>
  );
}

// GroupCard shows one group: an editable name, a live member roster with per-member role controls,
// and an "add member" picker over the users not yet in the group. Every membership change writes the
// affected user's full membership set immediately (no separate Save).
//
// A group owned by a LIVE directory rule is shown read-only: its name, roster and existence all come
// from ldap.yaml, and are recomputed from the directory on each member's next login. An edit here
// would either be silently reverted or - for a rename or delete - undone at the next boot, so we
// don't offer one. The API refuses these regardless; this only keeps the UI honest about it.
//
// An ORPHANED directory group (its rule was removed from the config) is fully editable: nothing
// syncs or recreates it, so it is the admins' to rename or clean up.
function GroupCard({ group, users }: { group: GroupView; users: UserView[] }) {
  const rename = useRenameGroup();
  const del = useDeleteGroup();
  const updateUser = useUpdateUser();
  const [name, setName] = useState(group.name);
  const managed = directoryLocked(group);
  const orphaned = directoryManaged(group) && !!group.orphaned;

  // Non-admin users only - admins have blanket access and carry no group roles. Members are the users
  // whose membership set includes this group; candidates are everyone else.
  const eligible = users.filter((u) => !u.is_admin);
  const members = eligible
    .map((u) => ({ user: u, role: u.memberships?.find((m) => m.group_id === group.id)?.role }))
    .filter((m): m is { user: UserView; role: GroupRole } => m.role !== undefined);
  const candidates = eligible.filter(
    (u) => !u.memberships?.some((m) => m.group_id === group.id),
  );

  const setMemberships = (u: UserView, memberships: GroupMembership[]) =>
    updateUser.mutate({ id: u.id, req: { memberships } });

  const addMember = (u: UserView) =>
    setMemberships(u, [...(u.memberships ?? []), { group_id: group.id, role: 'read' }]);

  const removeMember = (u: UserView) =>
    setMemberships(u, (u.memberships ?? []).filter((m) => m.group_id !== group.id));

  const setRole = (u: UserView, role: GroupRole) =>
    setMemberships(
      u,
      (u.memberships ?? []).map((m) => (m.group_id === group.id ? { ...m, role } : m)),
    );

  const confirmDelete = () =>
    modals.openConfirmModal({
      title: `Delete group ${group.name}?`,
      centered: true,
      children: (
        <Text size="sm">
          Its {members.length} member(s) will be removed from the group. Their clusters are not
          affected.
        </Text>
      ),
      labels: { confirm: 'Delete group', cancel: 'Cancel' },
      confirmProps: { color: 'red' },
      onConfirm: () => del.mutate(group.id),
    });

  return (
    <Card radius="md" padding="lg">
      <Group justify="space-between" wrap="nowrap" mb="md">
        <Group gap="sm" wrap="nowrap" style={{ flex: 1, minWidth: 0 }}>
          <ThemeIcon size={38} radius="md" variant="light" color="brand">
            <IconUsersGroup size={20} stroke={1.6} />
          </ThemeIcon>
          <div style={{ flex: 1, minWidth: 0 }}>
            {managed ? (
              <Group gap={6} wrap="nowrap">
                <Text fw={650} size="md" truncate>
                  {group.name}
                </Text>
                <Tooltip
                  label={`Managed by the directory (rule "${group.source_key}") - members and roles come from ldap.yaml`}
                  withArrow
                  multiline
                  w={260}
                >
                  <Badge size="xs" variant="light" color="gray" leftSection={<IconLock size={10} />}>
                    Directory
                  </Badge>
                </Tooltip>
              </Group>
            ) : (
              <TextInput
                variant="unstyled"
                value={name}
                onChange={(e) => setName(e.currentTarget.value)}
                onBlur={() => {
                  const next = name.trim();
                  if (next && next !== group.name) rename.mutate({ id: group.id, name: next });
                  else setName(group.name);
                }}
                onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
                styles={{ input: { fontWeight: 650, fontSize: 16, minHeight: 0, height: 24 } }}
                aria-label="Group name"
              />
            )}
            <Group gap={6} wrap="nowrap">
              <Text size="xs" c="dimmed">
                {members.length === 0 ? 'No members' : `${members.length} member${members.length === 1 ? '' : 's'}`}
              </Text>
              {orphaned && (
                <Tooltip
                  label={`No mapping rule claims "${group.source_key}" any more. Nothing syncs this group - delete it, or restore its rule in ldap.yaml.`}
                  withArrow
                  multiline
                  w={280}
                >
                  <Badge size="xs" variant="light" color="orange">
                    rule removed
                  </Badge>
                </Tooltip>
              )}
            </Group>
          </div>
        </Group>
        {!managed && (
          <Tooltip label="Delete group" withArrow>
            <ActionIcon variant="subtle" color="red" onClick={confirmDelete}>
              <IconTrash size={16} />
            </ActionIcon>
          </Tooltip>
        )}
      </Group>

      {members.length > 0 && (
        <Stack gap={4} mb="sm">
          {members.map(({ user, role }) => (
            <MemberRow
              key={user.id}
              user={user}
              role={role}
              readOnly={managed}
              pending={updateUser.isPending}
              onRole={(r) => setRole(user, r)}
              onRemove={() => removeMember(user)}
            />
          ))}
        </Stack>
      )}

      {!managed && (
        <>
          <Divider my="sm" variant="dashed" />
          <AddMember candidates={candidates} pending={updateUser.isPending} onAdd={addMember} />
        </>
      )}
      {managed && members.length === 0 && (
        <Text size="xs" c="dimmed" fs="italic">
          Members appear here as they sign in and match this group's rule.
        </Text>
      )}
    </Card>
  );
}

function initials(username: string) {
  return username.slice(0, 2).toUpperCase();
}

// MemberRow is one member inside a GroupCard: avatar + name, a Read/Write role toggle, and a remove
// button. The role toggle and removal both apply immediately.
//
// readOnly is set for a directory-managed group, where both the membership and the role come from a
// mapping rule and are recomputed on the member's next login.
function MemberRow({
  user,
  role,
  pending,
  readOnly,
  onRole,
  onRemove,
}: {
  user: UserView;
  role: GroupRole;
  pending: boolean;
  readOnly?: boolean;
  onRole: (role: GroupRole) => void;
  onRemove: () => void;
}) {
  return (
    <Group
      justify="space-between"
      wrap="nowrap"
      gap="xs"
      px="xs"
      py={6}
      style={{ borderRadius: 8 }}
    >
      <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
        <Avatar size={28} radius="xl" color="brand" variant="light">
          <Text size="xs" fw={600}>
            {initials(user.username)}
          </Text>
        </Avatar>
        <Text size="sm" fw={500} truncate>
          {user.username}
        </Text>
      </Group>
      <Group gap={6} wrap="nowrap">
        {readOnly ? (
          <Tooltip label="Role comes from the directory mapping rule" withArrow>
            <Badge
              size="sm"
              variant="light"
              color={role === 'write' ? 'brand' : 'gray'}
              leftSection={role === 'write' ? <IconPencil size={11} /> : <IconEye size={11} />}
            >
              {role === 'write' ? 'Write' : 'Read'}
            </Badge>
          </Tooltip>
        ) : (
          <>
            <SegmentedControl
              size="xs"
              value={role}
              onChange={(v) => onRole(v as GroupRole)}
              disabled={pending}
              data={[
                {
                  value: 'read',
                  label: (
                    <Group gap={4} wrap="nowrap" justify="center">
                      <IconEye size={13} />
                      <span>Read</span>
                    </Group>
                  ),
                },
                {
                  value: 'write',
                  label: (
                    <Group gap={4} wrap="nowrap" justify="center">
                      <IconPencil size={13} />
                      <span>Write</span>
                    </Group>
                  ),
                },
              ]}
            />
            <Tooltip label="Remove from group" withArrow>
              <ActionIcon variant="subtle" color="red" onClick={onRemove} disabled={pending}>
                <IconX size={15} />
              </ActionIcon>
            </Tooltip>
          </>
        )}
      </Group>
    </Group>
  );
}

// AddMember is the per-group user picker: a searchable Select over the accounts not yet in the group.
// Picking one adds them at the least-privileged Read role.
function AddMember({
  candidates,
  pending,
  onAdd,
}: {
  candidates: UserView[];
  pending: boolean;
  onAdd: (u: UserView) => void;
}) {
  const [value, setValue] = useState<string | null>(null);

  if (candidates.length === 0) {
    return (
      <Text size="xs" c="dimmed" ta="center">
        Every account is already a member.
      </Text>
    );
  }

  return (
    <Select
      placeholder="Add a user to this group…"
      leftSection={<IconUserPlus size={16} />}
      data={candidates.map((u) => ({ value: u.id, label: u.username }))}
      value={value}
      onChange={(id) => {
        const u = candidates.find((c) => c.id === id);
        if (u) onAdd(u);
        setValue(null); // reset so the box stays a fresh "add" affordance
      }}
      disabled={pending}
      searchable
      size="xs"
      nothingFoundMessage="No matching users"
    />
  );
}

function UserRow({
  u,
  selfId,
  groups,
  providers,
  sharedQuota,
}: {
  u: UserView;
  selfId?: string;
  groups: GroupView[];
  providers: string[];
  sharedQuota: boolean;
}) {
  const updateUser = useUpdateUser();
  const del = useDeleteUser();

  // A grant per infrastructure. The server merges what we send, but we always send the full set the
  // deployment offers, so the Save button means exactly what the row shows.
  const granted = (p: string): ResourceQuota => u.quotas?.[p] ?? { vcpu: 0, mem_mb: 0, disk_gb: 0 };
  const [draft, setDraft] = useState<Record<string, ResourceQuota>>(() =>
    Object.fromEntries(providers.map((p) => [p, granted(p)])),
  );
  const setField = (p: string, field: keyof ResourceQuota, v: number) =>
    setDraft((d) => ({ ...d, [p]: { ...d[p], [field]: v } }));

  const dirty = providers.some(
    (p) =>
      draft[p]?.vcpu !== granted(p).vcpu ||
      draft[p]?.mem_mb !== granted(p).mem_mb ||
      draft[p]?.disk_gb !== granted(p).disk_gb,
  );
  const isSelf = u.id === selfId;

  const confirmDelete = () =>
    modals.openConfirmModal({
      title: `Delete ${u.username}?`,
      centered: true,
      children: (
        <Text size="sm">
          This removes the account and <b>tears down all {u.cluster_count} of its clusters</b> (their
          VMs are destroyed). This cannot be undone.
        </Text>
      ),
      labels: { confirm: 'Delete account', cancel: 'Cancel' },
      confirmProps: { color: 'red' },
      onConfirm: () => del.mutate(u.id),
    });

  return (
    <Table.Tr>
      <Table.Td>
        <Group gap="xs">
          <Text fw={600}>{u.username}</Text>
          {u.is_admin && (
            <Badge color="grape" variant="light" size="sm" leftSection={<IconShieldCheck size={12} />}>
              admin
            </Badge>
          )}
          {isSelf && (
            <Badge color="gray" variant="light" size="sm">
              you
            </Badge>
          )}
          {fromDirectory(u) && (
            <Tooltip
              label="Provisioned from the directory on first sign-in; no password is stored here"
              withArrow
            >
              <Badge
                color="gray"
                variant="light"
                size="sm"
                leftSection={<IconAddressBook size={12} />}
              >
                directory
              </Badge>
            </Tooltip>
          )}
        </Group>
        {/* Directory accounts are named by sAMAccountName, which on its own tells an admin very
            little about who they're granting capacity to. */}
        {u.display_name && (
          <Text size="xs" c="dimmed">
            {u.display_name}
            {u.email ? ` · ${u.email}` : ''}
          </Text>
        )}
      </Table.Td>
      <Table.Td>
        {u.is_admin ? (
          <Tooltip label="Admins have full access to every cluster; group roles don't apply">
            <Text size="xs" c="dimmed">
              -
            </Text>
          </Tooltip>
        ) : (
          <MembershipBadges memberships={u.memberships ?? []} groups={groups} />
        )}
      </Table.Td>
      <Table.Td>
        <Text size="sm">{u.cluster_count}</Text>
      </Table.Td>
      <Table.Td>
        {u.is_admin ? (
          <Badge variant="light" color="grape" size="sm">
            {sharedQuota
              ? 'Shares each infrastructure’s full ceiling with everyone'
              : 'Automatic - draws from each infrastructure’s unallocated pool'}
          </Badge>
        ) : (
          <Stack gap={6}>
            {providers.map((p) => {
              const used = u.usage?.[p] ?? { vcpu: 0, mem_mb: 0, disk_gb: 0 };
              return (
                <Group key={p} gap="xs" wrap="nowrap" align="center">
                  <Badge
                    variant="light"
                    color={p === 'vsphere' ? 'indigo' : 'teal'}
                    size="sm"
                    radius="sm"
                    w={110}
                  >
                    {providerLabel(p)}
                  </Badge>
                  {sharedQuota ? (
                    // Shared pool: no grant to edit, just what this account is drawing from it.
                    <Text size="xs" c="dimmed">
                      {used.vcpu} vCPU · {gib(used.mem_mb)} GiB · {used.disk_gb} GB disk in use
                    </Text>
                  ) : (
                    <>
                      <NumberInput
                        value={draft[p]?.vcpu ?? 0}
                        onChange={(v) => setField(p, 'vcpu', Number(v) || 0)}
                        min={0}
                        step={1}
                        w={92}
                        size="xs"
                        suffix=" vCPU"
                      />
                      <NumberInput
                        value={draft[p]?.mem_mb ?? 0}
                        onChange={(v) => setField(p, 'mem_mb', Number(v) || 0)}
                        min={0}
                        step={1024}
                        w={116}
                        size="xs"
                        suffix=" MiB"
                      />
                      {/* Storage is granted like cores and memory: it is what a pool's root-disk
                          override and a node's extra disks actually spend. */}
                      <NumberInput
                        value={draft[p]?.disk_gb ?? 0}
                        onChange={(v) => setField(p, 'disk_gb', Number(v) || 0)}
                        min={0}
                        step={100}
                        w={110}
                        size="xs"
                        suffix=" GB"
                      />
                      <Text size="xs" c="dimmed" w={180}>
                        {used.vcpu} vCPU · {gib(used.mem_mb)} GiB · {used.disk_gb} GB in use
                      </Text>
                    </>
                  )}
                </Group>
              );
            })}
          </Stack>
        )}
      </Table.Td>
      <Table.Td>
        <Text size="sm" c="dimmed">
          {relative(u.created_at)}
        </Text>
      </Table.Td>
      <Table.Td>
        <Group gap={4} justify="flex-end" wrap="nowrap">
          {!u.is_admin && !sharedQuota && (
            <Button
              size="xs"
              variant="light"
              leftSection={<IconDeviceFloppy size={14} />}
              disabled={!dirty}
              loading={updateUser.isPending}
              onClick={() => updateUser.mutate({ id: u.id, req: { quotas: draft } })}
            >
              Save
            </Button>
          )}
          <Tooltip label={isSelf ? "You can't delete your own account" : 'Delete account'}>
            <ActionIcon variant="subtle" color="red" onClick={confirmDelete} disabled={isSelf}>
              <IconTrash size={16} />
            </ActionIcon>
          </Tooltip>
        </Group>
      </Table.Td>
    </Table.Tr>
  );
}

// MembershipBadges renders a user's groups & roles read-only on the Users tab; assignment itself now
// lives on the Groups tab (a per-group roster reads more naturally than a cramped inline editor).
function MembershipBadges({
  memberships,
  groups,
}: {
  memberships: GroupMembership[];
  groups: GroupView[];
}) {
  const nameOf = (id: string) => groups.find((g) => g.id === id)?.name ?? id;

  if (memberships.length === 0) {
    return (
      <Text size="xs" c="dimmed">
        No groups
      </Text>
    );
  }

  return (
    <Group gap={6} maw={260}>
      {memberships.map((m) => (
        <Tooltip
          key={m.group_id}
          label={m.role === 'write' ? 'Write - full management' : 'Read - view-only'}
          withArrow
        >
          <Badge
            variant="light"
            color={m.role === 'write' ? 'brand' : 'gray'}
            size="sm"
            leftSection={m.role === 'write' ? <IconPencil size={11} /> : <IconEye size={11} />}
            style={{ textTransform: 'none' }}
          >
            {nameOf(m.group_id)}
          </Badge>
        </Tooltip>
      ))}
    </Group>
  );
}
