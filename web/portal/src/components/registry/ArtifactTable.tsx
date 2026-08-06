import { Table, Badge, Group, Text, Code, Skeleton } from '@mantine/core';
import { useRegistryArtifacts } from '../../lib/queries';
import { relative } from '../../lib/format';

// ArtifactTable lists one repository's images: their tags, size, and when they were pushed.
//
// It deliberately shows NO vulnerability summary. Every cluster runs the trivy-operator add-on and
// the Security page is the one place that answers "what is wrong with these images" - reading what
// is actually deployed rather than what a registry scanned at push time. A second, subtly different
// severity rollup here would only invite the two to be compared.

function bytes(n: number): string {
  if (n >= 1024 * 1024 * 1024) return `${(n / 1024 ** 3).toFixed(1)} GiB`;
  if (n >= 1024 * 1024) return `${Math.round(n / 1024 ** 2)} MiB`;
  return `${Math.round(n / 1024)} KiB`;
}

export function ArtifactTable({ project, repo }: { project: string; repo: string }) {
  const { data, isLoading } = useRegistryArtifacts(project, repo);

  if (isLoading) return <Skeleton height={90} radius="sm" />;
  if (!data || data.length === 0) {
    return (
      <Text size="sm" c="dimmed" py="sm">
        No images pushed to this repository yet.
      </Text>
    );
  }

  return (
    <Table striped highlightOnHover verticalSpacing="xs" fz="sm">
      <Table.Thead>
        <Table.Tr>
          <Table.Th>Tag</Table.Th>
          <Table.Th>Digest</Table.Th>
          <Table.Th>Size</Table.Th>
          <Table.Th>Pushed</Table.Th>
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {data.map((a) => (
          <Table.Tr key={a.digest}>
            <Table.Td>
              {a.tags && a.tags.length > 0 ? (
                <Group gap={4}>
                  {a.tags.map((t) => (
                    <Badge key={t} variant="light" size="sm">
                      {t}
                    </Badge>
                  ))}
                </Group>
              ) : (
                <Text size="xs" c="dimmed">
                  untagged
                </Text>
              )}
            </Table.Td>
            <Table.Td>
              {/* Short digest: the full one is 71 characters and nobody reads it in a table. */}
              <Code fz="xs">{a.digest.replace('sha256:', '').slice(0, 12)}</Code>
            </Table.Td>
            <Table.Td>{bytes(a.size_bytes)}</Table.Td>
            <Table.Td>{a.pushed_at ? relative(a.pushed_at) : '—'}</Table.Td>
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  );
}
