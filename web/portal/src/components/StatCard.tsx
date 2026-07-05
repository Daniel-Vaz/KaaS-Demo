import type { ReactNode } from 'react';
import { Paper, Group, Text, ThemeIcon } from '@mantine/core';
import type { Icon } from '@tabler/icons-react';

export function StatCard({
  icon: IconCmp,
  label,
  value,
  color = 'brand',
  sub,
}: {
  icon: Icon;
  label: string;
  value: ReactNode;
  color?: string;
  sub?: ReactNode;
}) {
  return (
    <Paper p="md" radius="md">
      <Group justify="space-between" align="flex-start" wrap="nowrap">
        <div>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
            {label}
          </Text>
          <Text fw={700} fz={28} lh={1.1} mt={4}>
            {value}
          </Text>
          {sub && (
            <Text size="xs" c="dimmed" mt={4}>
              {sub}
            </Text>
          )}
        </div>
        <ThemeIcon size={38} radius="md" variant="light" color={color}>
          <IconCmp size={20} stroke={1.6} />
        </ThemeIcon>
      </Group>
    </Paper>
  );
}
