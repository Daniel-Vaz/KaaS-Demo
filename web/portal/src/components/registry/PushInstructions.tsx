import { Code, CopyButton, ActionIcon, Tooltip, Stack, Text, Group } from '@mantine/core';
import { IconCopy, IconCheck } from '@tabler/icons-react';

// PushInstructions renders the three commands that get an image into a cluster's project. It exists
// because "you have a registry now" is useless without the prefix - the project name is
// platform-minted, so a user cannot guess it.

function CommandLine({ cmd }: { cmd: string }) {
  return (
    <Group gap="xs" wrap="nowrap" align="center">
      <Code block fz="xs" style={{ flex: 1, overflowX: 'auto' }}>
        {cmd}
      </Code>
      <CopyButton value={cmd}>
        {({ copied, copy }) => (
          <Tooltip label={copied ? 'Copied' : 'Copy'}>
            <ActionIcon variant="subtle" color={copied ? 'teal' : 'gray'} onClick={copy}>
              {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
            </ActionIcon>
          </Tooltip>
        )}
      </CopyButton>
    </Group>
  );
}

export function PushInstructions({
  host,
  pushPrefix,
  username,
}: {
  host: string;
  pushPrefix: string;
  username?: string;
}) {
  const login = username ? `docker login ${host} -u ${username}` : `docker login ${host}`;
  return (
    <Stack gap="xs">
      <Text size="sm" c="dimmed">
        Push an image to this cluster's project:
      </Text>
      <CommandLine cmd={login} />
      <CommandLine cmd={`docker tag myapp:latest ${pushPrefix}/myapp:latest`} />
      <CommandLine cmd={`docker push ${pushPrefix}/myapp:latest`} />
      <Text size="xs" c="dimmed">
        Workloads in this cluster pull from it with no extra configuration: the{' '}
        <Code fz="xs">kaas-registry</Code> pull secret is applied to the cluster's{' '}
        <Code fz="xs">default</Code> namespace, and the same credential is written to the cluster's
        Vault path so External Secrets can sync it into any other namespace.
      </Text>
    </Stack>
  );
}
