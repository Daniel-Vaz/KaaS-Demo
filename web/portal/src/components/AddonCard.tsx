import { Text, Badge, ThemeIcon, Tooltip, ActionIcon, Group, Code } from '@mantine/core';
import { IconCheck, IconLock, IconCode } from '@tabler/icons-react';
import type { Icon } from '@tabler/icons-react';
import classes from './AddonCard.module.css';

// cx joins truthy class names - a tiny local stand-in for clsx (not a project dependency).
const cx = (...parts: (string | false | undefined)[]) => parts.filter(Boolean).join(' ');

export interface AddonCardProps {
  name: string;
  version: string;
  description?: string;
  icon: Icon;
  color: string;
  selected: boolean;
  locked?: boolean; // pinned bundle default - always on, not toggleable
  edited?: boolean;
  updating?: boolean; // add-on is mid-reconcile on a running cluster
  chart?: string; // shown for custom-catalog add-ons
  onToggle?: () => void;
  onEditValues?: () => void;
  onViewDiff?: () => void;
}

// AddonCard is the interactive tile used across the create wizard's add-on step and the custom-
// catalog picker: a whole-card toggle with an icon, version, description, and an optional Helm-
// values editor affordance. Locked cards (bundle defaults) render on but can't be toggled off.
export function AddonCard({
  name,
  version,
  description,
  icon: IconCmp,
  color,
  selected,
  locked = false,
  edited = false,
  updating = false,
  chart,
  onToggle,
  onEditValues,
  onViewDiff,
}: AddonCardProps) {
  const toggle = () => {
    if (!locked) onToggle?.();
  };

  return (
    <div
      role={locked ? undefined : 'checkbox'}
      aria-checked={selected}
      tabIndex={locked ? undefined : 0}
      onClick={toggle}
      onKeyDown={(e) => {
        if (locked) return;
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          toggle();
        }
      }}
      className={cx(
        classes.card,
        !locked && classes.selectable,
        selected && classes.selected,
        locked && classes.locked,
      )}
    >
      <div className={classes.head}>
        <ThemeIcon size={38} radius="md" variant="light" color={color}>
          <IconCmp size={20} stroke={1.7} />
        </ThemeIcon>
        <div className={classes.titleWrap}>
          <Group gap={6} wrap="nowrap" mb={2}>
            <Text fw={600} size="sm" truncate>
              {name}
            </Text>
            <Badge size="xs" variant="default" radius="sm" style={{ flexShrink: 0 }}>
              {version}
            </Badge>
            {updating && (
              <Badge size="xs" variant="light" color="blue" radius="sm" style={{ flexShrink: 0 }}>
                updating…
              </Badge>
            )}
          </Group>
          {locked && (
            <Badge
              size="xs"
              variant="light"
              color="gray"
              radius="sm"
              leftSection={<IconLock size={9} />}
            >
              bundle default
            </Badge>
          )}
          {chart && !locked && (
            <Code style={{ fontSize: 11 }}>{chart}</Code>
          )}
        </div>
        <div
          className={cx(
            classes.pip,
            selected && !locked && classes.pipOn,
            locked && classes.pipLocked,
          )}
        >
          {(selected || locked) && <IconCheck size={13} stroke={3} />}
        </div>
      </div>

      {description && <Text className={classes.desc}>{description}</Text>}

      {(onEditValues || edited) && (
        <div className={classes.footer} onClick={(e) => e.stopPropagation()}>
          {edited ? (
            <Tooltip label="View changes vs. defaults" position="top" withArrow>
              <Badge
                size="xs"
                variant="light"
                color="yellow"
                radius="sm"
                style={{ cursor: 'pointer' }}
                onClick={onViewDiff}
              >
                values edited
              </Badge>
            </Tooltip>
          ) : (
            <span />
          )}
          {onEditValues && (
            <Tooltip
              label={selected ? 'Edit Helm values' : 'Select the add-on to edit its values'}
              position="left"
              withArrow
            >
              <ActionIcon
                variant="subtle"
                color="gray"
                size="sm"
                disabled={!selected}
                onClick={onEditValues}
              >
                <IconCode size={15} />
              </ActionIcon>
            </Tooltip>
          )}
        </div>
      )}
    </div>
  );
}
