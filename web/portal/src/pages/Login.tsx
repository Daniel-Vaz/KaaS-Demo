import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  Paper,
  TextInput,
  PasswordInput,
  Button,
  Title,
  Text,
  Stack,
  Group,
  Image,
  Anchor,
  Center,
  Alert,
} from '@mantine/core';
import { IconAlertCircle } from '@tabler/icons-react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { api, ApiError } from '../lib/api';
import { useAuth } from '../lib/auth';
import kaasLogo from '../assets/kaas-logo.png';

type Mode = 'login' | 'register';

// Login is the unauthenticated gate: sign in to an existing account or self-register a new one.
// New accounts start with zero quota, so a fresh tenant must ask an admin to grant capacity before
// creating clusters - the empty-quota state is surfaced on the Overview page.
//
// Whether registration is offered at all depends on the deployment: with directory auth
// (KAAS_AUTH=ldap) accounts are created from the directory on first login, so there is nothing to
// self-register. We ask the server rather than guessing, via the one public endpoint there is.
export function Login() {
  const { setUser } = useAuth();
  const navigate = useNavigate();
  const [mode, setMode] = useState<Mode>('login');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');

  const { data: authConfig } = useQuery({
    queryKey: ['auth-config'],
    queryFn: api.authConfig,
    staleTime: Infinity, // deployment-level config; it cannot change under a running server
  });
  // Default to the local shape while it loads, and if the call fails: showing the register link and
  // having it 403 is a better failure than hiding it on a deployment where it works.
  const directoryAuth = authConfig?.mode === 'ldap';
  const canRegister = authConfig?.registration_enabled ?? true;

  const submit = useMutation({
    mutationFn: () =>
      mode === 'login' ? api.login(username, password) : api.register(username, password),
    onSuccess: (u) => {
      setUser(u);
      // Land on the dashboard rather than wherever the URL happened to point (e.g. a cluster a
      // previous session was viewing that this account can't, or can now unexpectedly, see).
      navigate('/', { replace: true });
    },
  });

  const err = submit.error instanceof ApiError ? submit.error.message : submit.error ? String(submit.error) : null;

  return (
    <Center mih="100vh" p="md">
      <Paper withBorder shadow="md" p="xl" radius="md" w={{ base: '100%', xs: 400 }}>
        <Stack gap="lg">
          <Group gap="sm" justify="center">
            <Image src={kaasLogo} alt="KaaS" w={96} h={96} radius="md" fit="contain" />
            <div>
              <Title order={3} lh={1}>
                KubeHarbor
              </Title>
              <Text size="xs" c="dimmed" lh={1}>
                Kubernetes Without the Rough Seas
              </Text>
            </div>
          </Group>

          <Stack gap={4}>
            <Title order={4} ta="center">
              {mode === 'login' ? 'Sign in' : 'Create an account'}
            </Title>
            {directoryAuth && mode === 'login' && (
              <Text size="xs" c="dimmed" ta="center">
                Use your organisation account
              </Text>
            )}
          </Stack>

          <form
            onSubmit={(e) => {
              e.preventDefault();
              submit.mutate();
            }}
          >
            <Stack gap="sm">
              {err && (
                <Alert color="red" icon={<IconAlertCircle size={16} />} variant="light">
                  {err}
                </Alert>
              )}
              <TextInput
                label="Username"
                placeholder="alice"
                required
                value={username}
                onChange={(e) => setUsername(e.currentTarget.value)}
                autoFocus
                data-autofocus
              />
              <PasswordInput
                label="Password"
                placeholder="••••••••"
                required
                value={password}
                onChange={(e) => setPassword(e.currentTarget.value)}
              />
              <Button type="submit" loading={submit.isPending} fullWidth mt="xs">
                {mode === 'login' ? 'Sign in' : 'Register'}
              </Button>
            </Stack>
          </form>

          {canRegister && (
            <Text size="sm" c="dimmed" ta="center">
              {mode === 'login' ? "Don't have an account? " : 'Already have an account? '}
              <Anchor
                component="button"
                type="button"
                onClick={() => {
                  setMode(mode === 'login' ? 'register' : 'login');
                  submit.reset();
                }}
              >
                {mode === 'login' ? 'Register' : 'Sign in'}
              </Anchor>
            </Text>
          )}
        </Stack>
      </Paper>
    </Center>
  );
}
