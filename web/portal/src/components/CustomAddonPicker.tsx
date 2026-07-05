import { useMemo } from 'react';
import { Stack, Group, Text, Anchor, ThemeIcon, SimpleGrid } from '@mantine/core';
import { Link } from 'react-router-dom';
import { IconPackages } from '@tabler/icons-react';
import { useCustomCatalogs } from '../lib/queries';
import { addonMeta } from '../lib/addonMeta';
import { AddonCard } from './AddonCard';
import type { CustomAddonRef } from '../lib/types';

// keyOf encodes a (catalog, add-on) pair as one selection key.
const keyOf = (catalogId: string, name: string) => `${catalogId}::${name}`;

// CustomAddonPicker renders the user's visible custom-catalog add-ons, grouped by catalog, as
// selectable cards. Selection is a list of {catalog_id, name} refs. Shared by the create wizard and
// the cluster Add-ons tab.
export function CustomAddonPicker({
  selected,
  onChange,
}: {
  selected: CustomAddonRef[];
  onChange: (next: CustomAddonRef[]) => void;
}) {
  const { data: catalogs } = useCustomCatalogs();

  const selectedKeys = useMemo(
    () => new Set(selected.map((r) => keyOf(r.catalog_id, r.name))),
    [selected],
  );
  const withAddons = (catalogs ?? []).filter((c) => (c.addons?.length ?? 0) > 0);

  if (!catalogs) return null;
  if (withAddons.length === 0) {
    return (
      <Text size="sm" c="dimmed">
        No custom-catalog add-ons available. Define some under{' '}
        <Anchor component={Link} to="/catalog">
          Catalog → Custom catalogs
        </Anchor>
        .
      </Text>
    );
  }

  const toggle = (catalogId: string, name: string) => {
    const key = keyOf(catalogId, name);
    const next = selectedKeys.has(key)
      ? selected.filter((r) => keyOf(r.catalog_id, r.name) !== key)
      : [...selected, { catalog_id: catalogId, name }];
    onChange(next);
  };

  return (
    <Stack gap="lg">
      {withAddons.map((cc) => (
        <div key={cc.id}>
          <Group gap={6} mb="xs">
            <ThemeIcon size={20} radius="sm" variant="light" color="brand">
              <IconPackages size={12} />
            </ThemeIcon>
            <Text size="sm" fw={600}>
              {cc.name}
            </Text>
            <Text size="xs" c="dimmed">
              {cc.owner_username}
            </Text>
          </Group>
          <SimpleGrid cols={{ base: 1, sm: 2, xl: 3 }} spacing="sm">
            {(cc.addons ?? []).map((ad) => {
              const meta = addonMeta(ad.name);
              return (
                <AddonCard
                  key={ad.name}
                  name={ad.name}
                  version={ad.version}
                  description={ad.description}
                  chart={ad.chart}
                  icon={meta.icon}
                  color={meta.color}
                  selected={selectedKeys.has(keyOf(cc.id, ad.name))}
                  onToggle={() => toggle(cc.id, ad.name)}
                />
              );
            })}
          </SimpleGrid>
        </div>
      ))}
    </Stack>
  );
}
