import { Card, Group, Text, Stack, Progress, ThemeIcon, Loader, Badge, Code } from '@mantine/core';
import { IconCircleCheck, IconArrowUpCircle, IconClockHour4 } from '@tabler/icons-react';
import { duration, secondsSince } from '../lib/format';
import { provisionedNodeCount, workerCount } from '../lib/cluster';
import type { Cluster, Bundle, Operation } from '../lib/types';

// One component the upgrade converges, mirroring the reconciler's per-component dispatch order
// (OS roll → in-place Kubernetes → helm CNI/add-ons). `done` is derived from the live cluster
// provenance matching the target bundle, so steps tick off as the reconciler advances.
interface UpgradeStep {
  key: string;
  label: string;
  target: string;
  strategy: string;
  done: boolean;
  // rough per-step time budget (seconds) used only for the estimate; clearly a heuristic.
  estimate: number;
}

function upgradeSteps(c: Cluster, target: Bundle): UpgradeStep[] {
  const nodes = provisionedNodeCount(c) || c.control_planes + workerCount(c);
  const steps: UpgradeStep[] = [
    {
      key: 'os',
      label: 'OS image',
      target: target.os,
      strategy: 'rolling node replacement',
      done: c.os_image === target.os,
      estimate: nodes * 90,
    },
    {
      key: 'k8s',
      label: 'Kubernetes',
      target: target.kubernetes,
      strategy: 'in-place kubeadm upgrade',
      done: c.k8s_version === target.kubernetes,
      estimate: 120,
    },
  ];
  const cniTarget = target.addons[target.cni];
  steps.push({
    key: 'cni',
    label: `CNI (${target.cni})`,
    target: `${target.cni} ${cniTarget}`,
    strategy: 'helm upgrade',
    done: c.cni === target.cni && c.cni_version === cniTarget,
    estimate: 45,
  });
  // Only add-ons the cluster actually runs are touched by an upgrade.
  const running = new Map((c.addons ?? []).filter((a) => a.phase !== 'removing').map((a) => [a.name, a.version] as const));
  for (const [name, ver] of Object.entries(target.addons)) {
    if (name === target.cni) continue;
    const cur = running.get(name);
    if (cur === undefined) continue;
    steps.push({
      key: `addon:${name}`,
      label: `add-on ${name}`,
      target: ver,
      strategy: 'helm upgrade',
      done: cur === ver,
      estimate: 45,
    });
  }
  return steps;
}

// StepIcon: check when converged, spinner for the step currently converging, dimmed dot otherwise.
function StepIcon({ done, active }: { done: boolean; active: boolean }) {
  if (done) {
    return (
      <ThemeIcon variant="light" color="teal" radius="xl" size={28}>
        <IconCircleCheck size={16} />
      </ThemeIcon>
    );
  }
  if (active) {
    return (
      <ThemeIcon variant="light" color="cyan" radius="xl" size={28}>
        <Loader size={14} color="cyan" />
      </ThemeIcon>
    );
  }
  return (
    <ThemeIcon variant="light" color="gray" radius="xl" size={28}>
      <IconArrowUpCircle size={16} />
    </ThemeIcon>
  );
}

// NodeImageMix summarizes how many VMs run each golden image - the live signal of a rolling OS
// replacement advancing one node at a time.
function NodeImageMix({ cluster }: { cluster: Cluster }) {
  const byImage = new Map<string, number>();
  for (const n of cluster.nodes ?? []) {
    const img = n.image || 'unknown';
    byImage.set(img, (byImage.get(img) ?? 0) + 1);
  }
  if (byImage.size <= 1) return null;
  return (
    <Group gap="xs" mt={4}>
      {[...byImage.entries()].map(([img, count]) => (
        <Badge key={img} size="xs" variant="outline" color="cyan">
          {count}× {img}
        </Badge>
      ))}
    </Group>
  );
}

// UpgradeProgress shows the evolution of an in-flight bundle promotion as a converging checklist,
// with a live time estimate. `target` is the final bundle; `op` is the in-progress upgrade record
// (for the elapsed clock). Multi-hop upgrades converge against the final target naturally.
export function UpgradeProgress({
  cluster,
  target,
  op,
}: {
  cluster: Cluster;
  target: Bundle;
  op?: Operation;
}) {
  const steps = upgradeSteps(cluster, target);
  const activeKey = steps.find((s) => !s.done)?.key;
  const doneCount = steps.filter((s) => s.done).length;
  const remaining = steps.filter((s) => !s.done).reduce((sum, s) => sum + s.estimate, 0);
  const elapsed = op ? secondsSince(op.started_at) : 0;

  return (
    <Card radius="md" padding="lg" withBorder>
      <Group justify="space-between" mb="xs" wrap="nowrap">
        <Group gap="xs">
          <IconArrowUpCircle size={18} />
          <Text fw={600}>Upgrading to {cluster.target_bundle || target.name}</Text>
        </Group>
        <Group gap="xs" c="dimmed">
          <IconClockHour4 size={14} />
          <Text size="xs">
            {op ? `elapsed ${duration(elapsed)}` : 'in progress'}
            {remaining > 0 ? ` · ~${duration(remaining)} remaining` : ''}
          </Text>
        </Group>
      </Group>

      <Progress
        value={steps.length ? (doneCount / steps.length) * 100 : 0}
        color="teal"
        radius="xl"
        size="sm"
        mb="md"
        striped={remaining > 0}
        animated={remaining > 0}
      />

      <Stack gap="sm">
        {steps.map((s) => {
          const active = s.key === activeKey;
          return (
            <Group key={s.key} wrap="nowrap" align="flex-start" gap="sm">
              <StepIcon done={s.done} active={active} />
              <div style={{ flex: 1, minWidth: 0 }}>
                <Group gap={6} wrap="nowrap">
                  <Text size="sm" fw={500} c={s.done ? undefined : active ? undefined : 'dimmed'}>
                    {s.label}
                  </Text>
                  <Text size="xs" c="dimmed">
                    → <Code>{s.target}</Code>
                  </Text>
                  {active && (
                    <Badge size="xs" variant="light" color="cyan">
                      {s.strategy}
                    </Badge>
                  )}
                </Group>
                {active && s.key === 'os' && <NodeImageMix cluster={cluster} />}
              </div>
            </Group>
          );
        })}
      </Stack>

      <Text size="xs" c="dimmed" mt="md">
        Time remaining is a rough estimate. Follow every step live in the <b>Activity</b> tab.
      </Text>
    </Card>
  );
}
