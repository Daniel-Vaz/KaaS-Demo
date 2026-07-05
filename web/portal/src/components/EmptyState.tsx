import type { ReactNode } from 'react';
import { Stack, ThemeIcon, Text, Center } from '@mantine/core';
import type { Icon } from '@tabler/icons-react';

export function EmptyState({
  icon: IconCmp,
  title,
  description,
  action,
}: {
  icon: Icon;
  title: string;
  description?: string;
  action?: ReactNode;
}) {
  return (
    <Center py={48}>
      <Stack align="center" gap="xs" maw={420}>
        <ThemeIcon size={54} radius="xl" variant="light" color="gray">
          <IconCmp size={28} stroke={1.5} />
        </ThemeIcon>
        <Text fw={600} size="lg">
          {title}
        </Text>
        {description && (
          <Text c="dimmed" ta="center" size="sm">
            {description}
          </Text>
        )}
        {action}
      </Stack>
    </Center>
  );
}
