import { useMemo } from 'react';
import {
  Modal,
  Stack,
  Group,
  Text,
  Badge,
  Alert,
  Loader,
  Center,
  useComputedColorScheme,
} from '@mantine/core';
import { IconGitCompare } from '@tabler/icons-react';
import CodeMirror from '@uiw/react-codemirror';
import { yaml } from '@codemirror/lang-yaml';
import { oneDark } from '@codemirror/theme-one-dark';
import { unifiedMergeView } from '@codemirror/merge';
import { EditorView } from '@codemirror/view';
import { useCatalogAddonValues, useClusterAddonValues } from '../lib/queries';

// AddonValuesDiff shows, read-only, what an add-on's customized values changed relative to the
// platform-curated defaults - the "before" is the effective (chart + catalog) values, the "after"
// is the user's override. It reuses the same values queries as the editor to fetch the baseline, and
// renders an inline (unified) diff with CodeMirror's merge view so removed lines are struck in red
// and added lines highlighted in green, matching the editor's IDE feel.
//
// Modes mirror AddonValuesEditor: pass `bundle` (wizard) or `clusterId` (Add-ons tab). `override` is
// the edited YAML to compare (the wizard draft or the saved cluster override).
export interface AddonValuesDiffProps {
  opened: boolean;
  onClose: () => void;
  addonName: string;
  addonVersion?: string;
  override: string;
  bundle?: string;
  clusterId?: string;
}

export function AddonValuesDiff(props: AddonValuesDiffProps) {
  const { opened, onClose, addonName, addonVersion, override, bundle, clusterId } = props;
  const manage = !!clusterId;

  const catalogQ = useCatalogAddonValues(addonName, bundle ?? '', opened && !manage);
  const clusterQ = useClusterAddonValues(clusterId, addonName, opened && manage);
  const data = manage ? clusterQ.data : catalogQ.data;
  const loading = manage ? clusterQ.isLoading : catalogQ.isLoading;
  const error = manage ? clusterQ.error : catalogQ.error;

  const dark = useComputedColorScheme('dark') === 'dark';
  const defaults = data?.effective_values ?? '';

  // Inline diff of the override against the platform defaults, read-only.
  const extensions = useMemo(
    () => [
      yaml(),
      EditorView.editable.of(false),
      unifiedMergeView({ original: defaults, mergeControls: false, gutter: true }),
    ],
    [defaults],
  );

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      size="xl"
      title={
        <Group gap={8}>
          <IconGitCompare size={18} />
          <Text fw={600}>Values diff - {addonName}</Text>
          {addonVersion && (
            <Badge size="sm" variant="default" radius="sm">
              {addonVersion}
            </Badge>
          )}
        </Group>
      }
    >
      <Stack>
        <Text size="sm" c="dimmed">
          Your customized values compared with the platform-curated defaults. Struck-through red lines
          were removed; highlighted lines are your additions.
        </Text>
        {loading ? (
          <Center py="xl">
            <Loader size="sm" />
          </Center>
        ) : error ? (
          <Alert color="red" variant="light">
            Could not load the default values: {String((error as Error).message)}
          </Alert>
        ) : (
          <CodeMirror
            // key forces a fresh editor when either side changes (unifiedMergeView reads `original`
            // at init, so remounting is the clean way to re-diff).
            key={`${defaults.length}:${override.length}`}
            value={override}
            height="60vh"
            theme={dark ? oneDark : undefined}
            extensions={extensions}
            basicSetup={{ lineNumbers: true, foldGutter: false, highlightActiveLine: false }}
          />
        )}
      </Stack>
    </Modal>
  );
}
