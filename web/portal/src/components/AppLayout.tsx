import type { ReactNode } from 'react';
import { NavLink as RouterNavLink, useLocation, useNavigate, Link } from 'react-router';
import { useQueryClient } from '@tanstack/react-query';
import {
  AppShell,
  Burger,
  Group,
  Title,
  NavLink,
  ActionIcon,
  useMantineColorScheme,
  useComputedColorScheme,
  Image,
  Text,
  Box,
  ScrollArea,
  UnstyledButton,
  Menu,
  Avatar,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import {
  IconLayoutDashboard,
  IconServer2,
  IconStack2,
  IconDatabase,
  IconAffiliate,
  IconChartAreaLine,
  IconKey,
  IconPackages,
  IconBook2,
  IconSun,
  IconMoon,
  IconShieldCog,
  IconShieldHalf,
  IconLogout,
  IconUserCircle,
} from '@tabler/icons-react';
import kaasLogo from '../assets/kaas-logo.png';
import { useAuth } from '../lib/auth';
import { api } from '../lib/api';
import { useVersion } from '../lib/queries';
import type { GroupMembership } from '../lib/types';

// memberSubtitle summarizes a non-admin user's group standing for the account menu. Roles are now
// per-group, so we show how many groups they're in rather than a single role.
function memberSubtitle(memberships: GroupMembership[] | null | undefined): string {
  const n = memberships?.length ?? 0;
  if (n === 0) return 'Member';
  return `Member · ${n} group${n === 1 ? '' : 's'}`;
}

// VersionFooter names the release the API is running, at the foot of the sidebar. It reads GET
// /version (public, stamped at build time) rather than this bundle's own package.json, so it names
// the platform rather than the web image - the two are separate images and can legitimately differ
// during a rolling upgrade. The commit is a title attribute rather than visible text: it's what you
// need when reporting a bug, not something to read every day. Renders nothing until the fetch
// lands, and nothing at all if it fails - a missing footer is better than an error in the chrome.
function VersionFooter() {
  const { data } = useVersion();
  if (!data) return null;
  const commit = data.commit && data.commit !== 'unknown' ? data.commit.slice(0, 7) : null;
  return (
    <Box pt="xs" style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
      <Text size="xs" c="dimmed" ta="center" title={`commit ${data.commit} · built ${data.date}`}>
        KubeHarbor {data.version}
        {commit ? ` · ${commit}` : ''}
      </Text>
    </Box>
  );
}

const NAV = [
  { to: '/', label: 'Overview', icon: IconLayoutDashboard, end: true },
  { to: '/clusters', label: 'Clusters', icon: IconServer2, end: false },
  { to: '/workloads', label: 'Workloads', icon: IconStack2, end: false },
  { to: '/storage', label: 'Storage', icon: IconDatabase, end: false },
  { to: '/secrets', label: 'Secrets', icon: IconKey, end: false },
  { to: '/registry', label: 'Registry', icon: IconPackages, end: false },
  { to: '/networking', label: 'Networking', icon: IconAffiliate, end: false },
  { to: '/monitoring', label: 'Monitoring', icon: IconChartAreaLine, end: false },
  { to: '/security', label: 'Security', icon: IconShieldHalf, end: false },
  { to: '/catalog', label: 'Catalog', icon: IconBook2, end: true },
];

// ADMIN_NAV is appended for admin accounts (see App route gating).
const ADMIN_NAV = { to: '/admin', label: 'Administration', icon: IconShieldCog, end: true };

function UserMenu() {
  const { user, setUser } = useAuth();
  const qc = useQueryClient();
  const navigate = useNavigate();
  if (!user) return null;

  const logout = async () => {
    try {
      await api.logout();
    } finally {
      // Flip the gate FIRST: setting `me` to null immediately re-renders <Login/>. Only then drop
      // every other cached query (deliberately not qc.clear(), which would also blow away the `me`
      // entry we just set and race a background refetch against it - the bug that made logout
      // appear to do nothing until a hard refresh).
      setUser(null);
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== 'me' });
      navigate('/', { replace: true });
    }
  };

  return (
    <Menu shadow="md" width={200} position="bottom-end">
      <Menu.Target>
        <UnstyledButton>
          <Group gap="xs" wrap="nowrap">
            <Avatar radius="xl" size={32} color="brand">
              {user.username.slice(0, 2).toUpperCase()}
            </Avatar>
            <Box visibleFrom="xs">
              <Text size="sm" fw={600} lh={1}>
                {user.username}
              </Text>
              <Text size="xs" c="dimmed" lh={1}>
                {user.is_admin ? 'Administrator' : memberSubtitle(user.memberships)}
              </Text>
            </Box>
          </Group>
        </UnstyledButton>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Label>{user.username}</Menu.Label>
        <Menu.Item component={Link} to="/profile" leftSection={<IconUserCircle size={16} />}>
          Profile
        </Menu.Item>
        <Menu.Divider />
        <Menu.Item color="red" leftSection={<IconLogout size={16} />} onClick={logout}>
          Sign out
        </Menu.Item>
      </Menu.Dropdown>
    </Menu>
  );
}

function ThemeToggle() {
  const { setColorScheme } = useMantineColorScheme();
  const computed = useComputedColorScheme('dark', { getInitialValueInEffect: true });
  const isDark = computed === 'dark';
  return (
    <ActionIcon
      variant="default"
      size="lg"
      aria-label="Toggle color scheme"
      onClick={() => setColorScheme(isDark ? 'light' : 'dark')}
    >
      {isDark ? <IconSun size={18} /> : <IconMoon size={18} />}
    </ActionIcon>
  );
}

export function AppLayout({ children }: { children: ReactNode }) {
  const location = useLocation();
  const { user } = useAuth();
  const nav = user?.is_admin ? [...NAV, ADMIN_NAV] : NAV;
  // On mobile the navbar is an off-canvas drawer toggled by the header burger; on desktop
  // (>= sm) it is always visible and the burger is hidden.
  const [opened, { toggle, close }] = useDisclosure(false);

  return (
    <AppShell
      header={{ height: 60 }}
      navbar={{ width: 240, breakpoint: 'sm', collapsed: { mobile: !opened } }}
      padding={{ base: 'md', sm: 'lg' }}
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between" wrap="nowrap">
          <Group gap="xs" wrap="nowrap">
            <Burger opened={opened} onClick={toggle} hiddenFrom="sm" size="sm" aria-label="Toggle navigation" />
            <UnstyledButton component={Link} to="/" style={{ borderRadius: 8 }}>
              <Group gap="xs" wrap="nowrap">
                <Image src={kaasLogo} alt="KaaS" w={48} h={48} radius="md" fit="contain" />
                <Box visibleFrom="xs">
                  <Title order={4} lh={1}>
                    KubeHarbor
                  </Title>
                  <Text size="xs" c="dimmed" lh={1}>
                    Kubernetes Without the Rough Seas
                  </Text>
                </Box>
              </Group>
            </UnstyledButton>
          </Group>
          <Group gap="sm" wrap="nowrap">
            <ThemeToggle />
            <UserMenu />
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="sm">
        <AppShell.Section grow component={ScrollArea}>
          {nav.map((item) => {
            const active = item.end
              ? location.pathname === item.to
              : location.pathname.startsWith(item.to);
            return (
              <NavLink
                key={item.to}
                component={RouterNavLink}
                to={item.to}
                label={item.label}
                active={active}
                leftSection={<item.icon size={18} stroke={1.6} />}
                variant="light"
                mb={4}
                // Close the mobile drawer after picking a destination; a no-op on desktop.
                onClick={close}
              />
            );
          })}
        </AppShell.Section>
        <AppShell.Section>
          <VersionFooter />
        </AppShell.Section>
      </AppShell.Navbar>

      <AppShell.Main>{children}</AppShell.Main>
    </AppShell>
  );
}
