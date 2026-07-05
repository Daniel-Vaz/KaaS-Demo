import { useMemo, useState } from 'react';
import { Stack, Group, Button, Text, Alert, Divider, SimpleGrid } from '@mantine/core';
import { IconInfoCircle, IconDeviceFloppy, IconEye } from '@tabler/icons-react';
import { useUpdateCluster } from '../lib/queries';
import { dedupeAddons } from '../lib/catalog';
import { addonMeta, CATEGORY_ORDER } from '../lib/addonMeta';
import { AddonValuesEditor } from './AddonValuesEditor';
import { AddonValuesDiff } from './AddonValuesDiff';
import { AddonCard } from './AddonCard';
import { CustomAddonPicker } from './CustomAddonPicker';
import type { Cluster, Catalog, Addon, CatalogAddon, CustomAddonRef } from '../lib/types';

// sameRefs compares two custom-add-on selections irrespective of order.
function sameRefs(a: CustomAddonRef[], b: CustomAddonRef[]): boolean {
  if (a.length !== b.length) return false;
  const key = (r: CustomAddonRef) => `${r.catalog_id}::${r.name}`;
  const set = new Set(a.map(key));
  return b.every((r) => set.has(key(r)));
}

// Add/remove add-ons on a running cluster. Editing bumps the generation server-side; the
// reconciler converges the live cluster (level-triggered). canManage gates the write: a read-only
// group member sees the installed add-ons but can't change them (the server enforces this too).
export function ManagePanel({
  cluster,
  catalog,
  canManage,
}: {
  cluster: Cluster;
  catalog?: Catalog;
  canManage: boolean;
}) {
  const update = useUpdateCluster(cluster.id);

  const installed = useMemo(
    () => (cluster.addons ?? []).filter((a) => a.phase !== 'removing'),
    [cluster.addons],
  );
  // Built-in add-on names only - custom-catalog add-ons are reconciled via their own channel below.
  const currentAddons = useMemo(() => installed.filter((a) => !a.catalog_id).map((a) => a.name), [installed]);
  const byName = useMemo(() => {
    const m = new Map<string, Addon>();
    for (const a of installed) m.set(a.name, a);
    return m;
  }, [installed]);
  const options = useMemo(
    () => dedupeAddons((catalog?.addons ?? []).filter((a) => a.type === 'addon')),
    [catalog],
  );

  // Group the optional add-ons by cosmetic category so the picker reads as sections, mirroring the
  // create wizard's Add-ons step.
  const addonsByCategory = useMemo(() => {
    const m = new Map<string, CatalogAddon[]>();
    for (const a of options) {
      const cat = addonMeta(a.name).category;
      (m.get(cat) ?? m.set(cat, []).get(cat)!).push(a);
    }
    return CATEGORY_ORDER.filter((c) => m.has(c)).map((c) => [c, m.get(c)!] as const);
  }, [options]);

  // Currently-installed custom-catalog add-ons (self-contained on the cluster; carry catalog_id).
  const currentCustomRefs = useMemo<CustomAddonRef[]>(
    () =>
      installed
        .filter((a) => a.catalog_id)
        .map((a) => ({ catalog_id: a.catalog_id as string, name: a.name })),
    [installed],
  );

  const [addons, setAddons] = useState<string[]>(currentAddons);
  const [customAddons, setCustomAddons] = useState<CustomAddonRef[]>(currentCustomRefs);
  // Starting Helm values for add-ons being ADDED this edit (name -> YAML). Add-ons already on the
  // cluster edit their live override through the manage-mode editor instead (a PUT that reconciles).
  const [addonValues, setAddonValues] = useState<Record<string, string>>({});
  const [editing, setEditing] = useState<string | null>(null);
  const [diffing, setDiffing] = useState<string | null>(null);

  if (!canManage) {
    return (
      <Alert variant="light" color="gray" icon={<IconEye size={16} />}>
        Read-only access - add-on management is available to owners, group members with the{' '}
        <b>Write</b> role, and admins.
      </Alert>
    );
  }

  if (cluster.phase !== 'Ready') {
    return (
      <Alert variant="light" color="gray" icon={<IconInfoCircle size={16} />}>
        Add-on management becomes available once the cluster is <b>Ready</b>.
      </Alert>
    );
  }

  const builtinDirty = addons.length !== currentAddons.length || addons.some((a) => !currentAddons.includes(a));
  const customDirty = !sameRefs(customAddons, currentCustomRefs);
  const dirty = builtinDirty || customDirty;

  const toggle = (name: string) => {
    if (addons.includes(name)) {
      setAddons(addons.filter((n) => n !== name));
      // Dropping a not-yet-applied add-on discards any starting values drafted for it.
      if (addonValues[name] !== undefined) {
        const next = { ...addonValues };
        delete next[name];
        setAddonValues(next);
      }
    } else {
      setAddons([...addons, name]);
    }
  };

  const apply = () => {
    // Only send starting values for add-ons being newly added (the server applies them to
    // additions only; an existing add-on keeps its stored override).
    const newValues = Object.fromEntries(
      Object.entries(addonValues).filter(
        ([name]) => addons.includes(name) && !currentAddons.includes(name),
      ),
    );
    update.mutate({
      addons,
      ...(Object.keys(newValues).length ? { addon_values: newValues } : {}),
      // Only send the custom set when it changed, so it's never disturbed unintentionally.
      ...(customDirty ? { custom_addons: customAddons } : {}),
    });
  };

  // The add-on currently open in the editor/diff is "live" (on the cluster) or a brand-new pick.
  const editingLive = editing ? byName.get(editing) : undefined;
  const diffingLive = diffing ? byName.get(diffing) : undefined;

  return (
    <Stack>
      <Text size="sm" fw={500}>
        Add-ons
      </Text>
      {options.length === 0 && (
        <Text c="dimmed" size="sm">
          No optional add-ons in the catalog.
        </Text>
      )}
      {addonsByCategory.map(([category, items]) => (
        <div key={category}>
          <Text
            size="xs"
            fw={700}
            c="dimmed"
            tt="uppercase"
            mb="xs"
            style={{ letterSpacing: '0.04em' }}
          >
            {category}
          </Text>
          <SimpleGrid cols={{ base: 1, xl: 2 }} spacing="sm">
            {items.map((a) => {
              const live = byName.get(a.name);
              const meta = addonMeta(a.name);
              return (
                <AddonCard
                  key={a.name}
                  name={a.name}
                  version={live?.version ?? a.version}
                  description={a.description}
                  icon={meta.icon}
                  color={meta.color}
                  selected={addons.includes(a.name)}
                  edited={!!live?.values_override || !!addonValues[a.name]}
                  updating={live?.phase === 'updating'}
                  onToggle={() => toggle(a.name)}
                  onEditValues={() => setEditing(a.name)}
                  onViewDiff={() => setDiffing(a.name)}
                />
              );
            })}
          </SimpleGrid>
        </div>
      ))}

      <div>
        <Divider label="Custom catalog add-ons" labelPosition="left" mb="sm" />
        <CustomAddonPicker selected={customAddons} onChange={setCustomAddons} />
      </div>

      {editing &&
        (editingLive ? (
          // Installed add-on: edit its live override (a PUT that drives a helm upgrade).
          <AddonValuesEditor
            opened
            onClose={() => setEditing(null)}
            clusterId={cluster.id}
            addonName={editing}
            addonVersion={editingLive.version}
          />
        ) : (
          // Not-yet-installed add-on: draft its starting values into local state, applied on save.
          <AddonValuesEditor
            opened
            onClose={() => setEditing(null)}
            addonName={editing}
            addonVersion={options.find((a) => a.name === editing)?.version}
            bundle={cluster.bundle}
            initialOverride={addonValues[editing]}
            onSaveDraft={(override) => {
              const next = { ...addonValues };
              if (override === null) delete next[editing];
              else next[editing] = override;
              setAddonValues(next);
            }}
          />
        ))}

      {diffing &&
        (diffingLive ? (
          <AddonValuesDiff
            opened
            onClose={() => setDiffing(null)}
            clusterId={cluster.id}
            addonName={diffing}
            addonVersion={diffingLive.version}
            override={diffingLive.values_override ?? ''}
          />
        ) : (
          <AddonValuesDiff
            opened
            onClose={() => setDiffing(null)}
            addonName={diffing}
            addonVersion={options.find((a) => a.name === diffing)?.version}
            bundle={cluster.bundle}
            override={addonValues[diffing] ?? ''}
          />
        ))}

      <Group>
        <Button
          leftSection={<IconDeviceFloppy size={16} />}
          onClick={apply}
          loading={update.isPending}
          disabled={!dirty}
        >
          Apply changes
        </Button>
        {dirty && (
          <Text size="xs" c="dimmed">
            unsaved changes
          </Text>
        )}
      </Group>
    </Stack>
  );
}
