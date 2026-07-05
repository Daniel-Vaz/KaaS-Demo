import { useEffect, useMemo, useState } from 'react';
import {
  Modal,
  Stack,
  Group,
  Text,
  Badge,
  Button,
  Alert,
  Loader,
  Center,
  Spoiler,
  Code,
  useComputedColorScheme,
} from '@mantine/core';
import { IconAlertTriangle, IconDeviceFloppy, IconRotate, IconCode, IconInfoCircle } from '@tabler/icons-react';
import CodeMirror from '@uiw/react-codemirror';
import { yaml } from '@codemirror/lang-yaml';
import { oneDark } from '@codemirror/theme-one-dark';
import { useCatalogAddonValues, useClusterAddonValues, useSetClusterAddonValues } from '../lib/queries';
import type { AddonValuesView } from '../lib/types';

// AddonValuesEditor is the in-browser "IDE" for an add-on's Helm values, shared by the create wizard
// and the post-create Add-ons tab. It seeds a CodeMirror YAML editor with the chart's defaults merged
// with the platform's curated overrides (or the current per-cluster override), warns that the
// defaults are curated, and saves a full-document override.
//
// Two modes, chosen by which props are set:
//   - Draft (wizard): pass `bundle` + `initialOverride` + `onSaveDraft`. Save writes the edited YAML
//     back to the wizard's form state (or clears it when the content matches the defaults).
//   - Manage (Add-ons tab): pass `clusterId`. Save PUTs the override, driving a reconciler helm
//     upgrade. Reset sends an empty override (back to catalog defaults).
//   - Read-only (Catalog page): pass `readOnly`. Same catalog-scoped fetch as draft mode, but the
//     editor is view-only - no Save/Reset, no editing - so the catalog can show a chart's whole
//     default values (chart defaults + catalog overrides) without offering to change them.
export interface AddonValuesEditorProps {
  opened: boolean;
  onClose: () => void;
  addonName: string;
  addonVersion?: string;
  // Draft mode
  bundle?: string;
  initialOverride?: string;
  onSaveDraft?: (override: string | null) => void; // null = use platform defaults
  // Manage mode
  clusterId?: string;
  // Read-only mode (Catalog page): view the defaults, no editing.
  readOnly?: boolean;
}

export function AddonValuesEditor(props: AddonValuesEditorProps) {
  const { opened, onClose, addonName, addonVersion, clusterId, bundle, initialOverride, onSaveDraft, readOnly } = props;
  const manage = !!clusterId;

  const catalogQ = useCatalogAddonValues(addonName, bundle ?? '', opened && !manage);
  const clusterQ = useClusterAddonValues(clusterId, addonName, opened && manage);
  const data: AddonValuesView | undefined = manage ? clusterQ.data : catalogQ.data;
  const loading = manage ? clusterQ.isLoading : catalogQ.isLoading;
  const error = manage ? clusterQ.error : catalogQ.error;

  const save = useSetClusterAddonValues(clusterId ?? '');

  const dark = useComputedColorScheme('dark') === 'dark';
  const [content, setContent] = useState('');

  // The values a non-customized install would apply - the editor's "defaults" baseline.
  const defaults = data?.effective_values ?? '';
  // The value to seed the editor with when it opens: the saved override (manage) / wizard draft
  // (create) if present, else the defaults.
  const seed = useMemo(() => {
    const override = manage ? data?.override : initialOverride;
    return override && override.trim() ? override : defaults;
  }, [manage, data?.override, initialOverride, defaults]);

  // (Re)seed the editor whenever the modal opens or the source data resolves.
  useEffect(() => {
    if (opened && data) setContent(seed);
  }, [opened, data, seed]);

  const isDefault = content.trim() === defaults.trim();
  const dirty = content.trim() !== seed.trim();

  const handleSave = () => {
    if (manage) {
      save.mutate({ name: addonName, values: isDefault ? '' : content }, { onSuccess: onClose });
      return;
    }
    onSaveDraft?.(isDefault ? null : content);
    onClose();
  };

  const handleReset = () => {
    setContent(defaults);
    if (manage) {
      save.mutate({ name: addonName, values: '' }, { onSuccess: onClose });
    }
    // In draft mode, resetting just repopulates the editor; the user still confirms with Save.
  };

  const overrides = data?.catalog_overrides ?? {};
  const overrideKeys = Object.keys(overrides).sort();

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      size="xl"
      title={
        <Group gap={8}>
          <IconCode size={18} />
          <Text fw={600}>
            {readOnly ? 'Default values' : 'Edit values'} - {addonName}
          </Text>
          {addonVersion && (
            <Badge size="sm" variant="default" radius="sm">
              {addonVersion}
            </Badge>
          )}
          {!readOnly && !isDefault && (
            <Badge size="sm" color="yellow" variant="light">
              edited
            </Badge>
          )}
        </Group>
      }
    >
      <Stack>
        {readOnly ? (
          <Alert variant="light" color="gray" icon={<IconInfoCircle size={16} />}>
            These are the platform's <b>curated default values</b> for this add-on - the chart's own
            defaults merged with the platform's overrides. This is a read-only view; you customize
            them per-cluster when creating a cluster or from a cluster's Add-ons tab.
          </Alert>
        ) : (
          <Alert variant="light" color="yellow" icon={<IconAlertTriangle size={16} />}>
            These default values were <b>curated by the platform administrators</b>. Changing them can
            lead to unpredictable behaviour during cluster bootstrapping and ongoing operations - edit
            only if you know what you're doing.
          </Alert>
        )}

        {loading ? (
          <Center py="xl">
            <Loader size="sm" />
          </Center>
        ) : error ? (
          <Alert color="red" variant="light">
            Could not load the chart values: {String((error as Error).message)}
          </Alert>
        ) : (
          <>
            {overrideKeys.length > 0 && (
              <Spoiler maxHeight={0} showLabel="Show platform-curated overrides" hideLabel="Hide">
                <Code block>
                  {overrideKeys.map((k) => `${k} = ${overrides[k]}`).join('\n')}
                </Code>
              </Spoiler>
            )}
            <CodeMirror
              value={content}
              height="52vh"
              editable={!readOnly}
              theme={dark ? oneDark : undefined}
              extensions={[yaml()]}
              onChange={setContent}
              basicSetup={{ lineNumbers: true, foldGutter: true, highlightActiveLine: !readOnly }}
            />
            {!readOnly && (
              <Text size="xs" c="dimmed">
                {isDefault
                  ? 'Matches the platform defaults - saving keeps this add-on on the curated values.'
                  : 'Customized - saving stores this full values document for this cluster.'}
              </Text>
            )}
          </>
        )}

        {readOnly ? (
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Close
            </Button>
          </Group>
        ) : (
          <Group justify="space-between">
            <Button
              variant="subtle"
              color="gray"
              leftSection={<IconRotate size={16} />}
              onClick={handleReset}
              disabled={loading || (manage ? isDefault && !data?.override : isDefault)}
              loading={manage && save.isPending && isDefault}
            >
              Reset to defaults
            </Button>
            <Group>
              <Button variant="default" onClick={onClose}>
                Cancel
              </Button>
              <Button
                leftSection={<IconDeviceFloppy size={16} />}
                onClick={handleSave}
                disabled={loading || (manage ? !dirty : false)}
                loading={manage && save.isPending && !isDefault}
              >
                Save
              </Button>
            </Group>
          </Group>
        )}
      </Stack>
    </Modal>
  );
}
