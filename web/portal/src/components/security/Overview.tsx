// The Security Overview tab: cluster-wide posture at a glance - a per-kind KPI row (clicking a card
// jumps to that kind's tab), the most vulnerable images, and a per-namespace risk breakdown.

import { Grid, Card, Group, Text, Stack, Table, Box, Skeleton, Alert, UnstyledButton, Badge } from '@mantine/core';
import { IconAlertTriangle, IconShieldCheck } from '@tabler/icons-react';
import type { SecurityKind, SecurityOverview } from '../../lib/types';
import { countsTotal } from '../../lib/security';
import { SeverityBar, SeverityChips, SeverityLegend } from './severity';
import { EmptyState } from '../EmptyState';

export function Overview({
  data,
  isLoading,
  error,
  onSelectKind,
}: {
  data: SecurityOverview | undefined;
  isLoading: boolean;
  error: unknown;
  onSelectKind: (k: SecurityKind) => void;
}) {
  if (error) {
    return (
      <Alert color="red" icon={<IconAlertTriangle size={18} />} title="Could not load security overview">
        {error instanceof Error ? error.message : String(error)}
      </Alert>
    );
  }
  if (isLoading && !data) {
    return (
      <Grid>
        {[0, 1, 2, 3].map((i) => (
          <Grid.Col key={i} span={{ base: 12, sm: 6, md: 3 }}>
            <Skeleton height={128} radius="md" />
          </Grid.Col>
        ))}
        <Grid.Col span={{ base: 12, md: 7 }}>
          <Skeleton height={320} radius="md" />
        </Grid.Col>
        <Grid.Col span={{ base: 12, md: 5 }}>
          <Skeleton height={320} radius="md" />
        </Grid.Col>
      </Grid>
    );
  }

  const kinds = data?.kinds ?? [];
  const topImages = data?.top_images ?? [];
  const namespaces = data?.namespaces ?? [];
  const grandTotal = kinds.reduce((n, k) => n + countsTotal(k.totals), 0);

  return (
    <Stack gap="lg">
      <Grid>
        {kinds.map((k) => (
          <Grid.Col key={k.kind} span={{ base: 12, xs: 6, md: 3 }}>
            <UnstyledButton onClick={() => onSelectKind(k.kind)} style={{ display: 'block', width: '100%' }}>
              <Card padding="md" h="100%" style={{ transition: 'border-color 120ms' }} className="security-kpi">
                <Group justify="space-between" align="flex-start" wrap="nowrap">
                  <Text size="sm" fw={600} c="dimmed">
                    {k.title}
                  </Text>
                  <Text size="xs" c="dimmed">
                    {k.report_count} report{k.report_count === 1 ? '' : 's'}
                  </Text>
                </Group>
                <Text fw={800} fz={30} lh={1.1} mt={4}>
                  {countsTotal(k.totals)}
                </Text>
                <Box mt="xs">
                  <SeverityBar counts={k.totals} height={7} />
                </Box>
                <Box mt="xs">
                  <SeverityChips counts={k.totals} size="xs" />
                </Box>
              </Card>
            </UnstyledButton>
          </Grid.Col>
        ))}
      </Grid>

      {grandTotal === 0 ? (
        <EmptyState
          icon={IconShieldCheck}
          title="No findings"
          description="Trivy found nothing to report across this cluster. Either it's genuinely clean, or the operator is still completing its first scan."
        />
      ) : (
        <Grid>
          <Grid.Col span={{ base: 12, md: 7 }}>
            <Card padding="md" h="100%">
              <Group justify="space-between" mb="sm">
                <Text fw={700}>Most vulnerable images</Text>
                <SeverityLegend />
              </Group>
              {topImages.length === 0 ? (
                <Text size="sm" c="dimmed" py="lg" ta="center">
                  No image vulnerabilities found.
                </Text>
              ) : (
                <Table.ScrollContainer minWidth={420}>
                  <Table verticalSpacing="xs">
                    <Table.Tbody>
                      {topImages.map((img) => (
                        <Table.Tr key={img.image}>
                          <Table.Td>
                            <Text size="sm" fw={600} ff="monospace" truncate style={{ maxWidth: 260 }}>
                              {img.image}
                            </Text>
                            {img.workloads && img.workloads.length > 0 && (
                              <Text size="xs" c="dimmed" truncate style={{ maxWidth: 260 }}>
                                {img.workloads.join(', ')}
                              </Text>
                            )}
                          </Table.Td>
                          <Table.Td w={150}>
                            <SeverityBar counts={img.summary} />
                          </Table.Td>
                          <Table.Td w={140}>
                            <SeverityChips counts={img.summary} size="xs" />
                          </Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                </Table.ScrollContainer>
              )}
            </Card>
          </Grid.Col>

          <Grid.Col span={{ base: 12, md: 5 }}>
            <Card padding="md" h="100%">
              <Text fw={700} mb="sm">
                Risk by namespace
              </Text>
              <Stack gap="sm">
                {namespaces.length === 0 ? (
                  <Text size="sm" c="dimmed" py="lg" ta="center">
                    No namespaced findings.
                  </Text>
                ) : (
                  namespaces.map((ns) => (
                    <div key={ns.namespace}>
                      <Group justify="space-between" mb={4} wrap="nowrap">
                        <Badge variant="light" color="gray" size="sm">
                          {ns.namespace || 'cluster-scoped'}
                        </Badge>
                        <Text size="xs" c="dimmed">
                          {countsTotal(ns.totals)} findings
                        </Text>
                      </Group>
                      <SeverityBar counts={ns.totals} />
                    </div>
                  ))
                )}
              </Stack>
            </Card>
          </Grid.Col>
        </Grid>
      )}
    </Stack>
  );
}
