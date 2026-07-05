// The shared read-only YAML viewer: a syntax-highlighted, line-numbered code block, optionally with
// a copy/download toolbar. Used by the Workloads detail page and the Storage page's claim/class
// drawers - every "show me the object's YAML" surface in the portal renders through here.

import { useMemo } from 'react';
import { Box, Group, Tooltip, ActionIcon, CopyButton, Skeleton, Alert } from '@mantine/core';
import { IconCopy, IconCheck, IconDownload, IconAlertTriangle } from '@tabler/icons-react';
import { ApiError } from '../lib/api';
import { downloadText } from '../lib/download';

// ---- YAML syntax highlighting ------------------------------------------------

const YAML_COLORS = {
  gutter: '#484f58',
  punct: '#c9d1d9',
  key: '#79c0ff',
  string: '#a5d6ff',
  number: '#79c0ff',
  keyword: '#ff7b72',
  comment: '#8b949e',
} as const;

const YAML_MONO = 'ui-monospace, SFMono-Regular, Menlo, Monaco, "Liberation Mono", monospace';

type YamlToken = { text: string; color?: string };

function scalarTokens(raw: string): YamlToken[] {
  // Split off a trailing " # comment" (a naive but effective heuristic).
  let value = raw;
  let comment = '';
  const ci = raw.search(/\s#/);
  if (ci >= 0) {
    value = raw.slice(0, ci);
    comment = raw.slice(ci);
  }
  const tokens: YamlToken[] = [];
  const trimmed = value.trim();
  if (value) {
    let color: string = YAML_COLORS.string;
    if (/^(true|false|null|~|yes|no|on|off)$/i.test(trimmed)) color = YAML_COLORS.keyword;
    else if (/^-?\d+(\.\d+)?$/.test(trimmed)) color = YAML_COLORS.number;
    tokens.push({ text: value, color });
  }
  if (comment) tokens.push({ text: comment, color: YAML_COLORS.comment });
  return tokens;
}

function tokenizeYamlLine(line: string): YamlToken[] {
  const m = /^(\s*)(- )?(.*)$/.exec(line);
  if (!m) return [{ text: line }];
  const [, indent, dash, rest] = m;
  const tokens: YamlToken[] = [];
  if (indent) tokens.push({ text: indent });
  if (dash) tokens.push({ text: dash, color: YAML_COLORS.punct });

  if (rest.startsWith('#')) {
    tokens.push({ text: rest, color: YAML_COLORS.comment });
    return tokens;
  }
  const kv = /^("[^"]*"|'[^']*'|[\w.\-/]+)(:)(\s*)(.*)$/.exec(rest);
  if (kv) {
    tokens.push({ text: kv[1], color: YAML_COLORS.key });
    tokens.push({ text: kv[2], color: YAML_COLORS.punct });
    if (kv[3]) tokens.push({ text: kv[3] });
    if (kv[4]) tokens.push(...scalarTokens(kv[4]));
    return tokens;
  }
  tokens.push(...scalarTokens(rest));
  return tokens;
}

// YamlCode renders highlighted YAML with a line-number gutter. It always uses the dark code palette,
// in both portal themes - a code block reads as a terminal artifact, not as page chrome.
export function YamlCode({ yaml }: { yaml: string }) {
  const lines = useMemo(() => yaml.replace(/\n$/, '').split('\n'), [yaml]);
  const gutterWidth = `${String(lines.length).length + 1}ch`;
  return (
    <Box
      style={{
        display: 'flex',
        background: '#0d1117',
        borderRadius: 8,
        overflowX: 'auto',
        fontSize: 12.5,
        lineHeight: 1.55,
        fontFamily: YAML_MONO,
      }}
    >
      <Box
        aria-hidden
        style={{
          flex: '0 0 auto',
          textAlign: 'right',
          padding: '14px 10px',
          color: YAML_COLORS.gutter,
          background: '#0b0f14',
          userSelect: 'none',
          whiteSpace: 'pre',
        }}
      >
        {lines.map((_, i) => (
          <div key={i} style={{ minWidth: gutterWidth }}>
            {i + 1}
          </div>
        ))}
      </Box>
      <Box
        component="pre"
        style={{
          flex: 1,
          margin: 0,
          padding: '14px 16px',
          color: YAML_COLORS.punct,
          whiteSpace: 'pre',
          fontFamily: YAML_MONO,
        }}
      >
        {lines.map((line, i) => {
          const tokens = tokenizeYamlLine(line);
          return (
            <div key={i}>
              {tokens.length === 0
                ? ' '
                : tokens.map((t, j) => (
                    <span key={j} style={t.color ? { color: t.color } : undefined}>
                      {t.text}
                    </span>
                  ))}
            </div>
          );
        })}
      </Box>
    </Box>
  );
}

// YamlView is the whole YAML tab: the loading/error states of a manifest fetch, a copy/download
// toolbar, and the highlighted code. filename names the download (".yaml" is appended).
export function YamlView({
  yaml,
  filename,
  isLoading,
  error,
}: {
  yaml: string | undefined;
  filename: string;
  isLoading?: boolean;
  error?: unknown;
}) {
  if (isLoading && !yaml) return <Skeleton height={360} radius="md" />;
  if (error) {
    return (
      <Alert color="red" icon={<IconAlertTriangle size={18} />} title="Could not load YAML">
        {error instanceof ApiError ? error.message : String(error)}
      </Alert>
    );
  }
  const text = yaml ?? '';
  return (
    <Box>
      <Group justify="flex-end" mb="xs" gap="xs">
        <CopyButton value={text}>
          {({ copied, copy }) => (
            <Tooltip label={copied ? 'Copied' : 'Copy'}>
              <ActionIcon variant="light" color={copied ? 'teal' : 'gray'} onClick={copy}>
                {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
              </ActionIcon>
            </Tooltip>
          )}
        </CopyButton>
        <Tooltip label="Download">
          <ActionIcon
            variant="light"
            color="gray"
            onClick={() => downloadText(`${filename}.yaml`, text, 'application/yaml')}
          >
            <IconDownload size={16} />
          </ActionIcon>
        </Tooltip>
      </Group>
      <YamlCode yaml={text} />
    </Box>
  );
}
