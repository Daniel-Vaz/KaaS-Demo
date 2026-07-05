import { Stepper, Alert, Group, Text } from '@mantine/core';
import { useMediaQuery } from '@mantine/hooks';
import { IconCircleCheck, IconAlertTriangle, IconTrash } from '@tabler/icons-react';
import { PROVISION_STEPS, phaseMeta } from '../lib/phase';
import type { Cluster, Phase } from '../lib/types';

// Shows the cluster's progress along the happy-path provisioning sequence. Failed and Deleting
// clusters short-circuit to an alert rather than pretending to be on the ladder.
export function PhaseStepper({ cluster }: { cluster: Cluster }) {
  const phase = cluster.phase;
  // The 7-step horizontal ladder can't fit on a phone; stack it vertically there.
  const vertical = useMediaQuery('(max-width: 48em)');

  if (phase === 'Failed') {
    return (
      <Alert color="red" icon={<IconAlertTriangle size={18} />} title="Provisioning failed">
        {cluster.status || 'The reconciler could not converge this cluster. Check the Activity tab.'}
      </Alert>
    );
  }
  if (phase === 'Deleting' || phase === 'Deleted') {
    return (
      <Alert color="orange" icon={<IconTrash size={18} />} title="Deleting">
        This cluster is being torn down. Watch progress in the Activity tab.
      </Alert>
    );
  }

  const idx = PROVISION_STEPS.indexOf(phase as Phase);
  const active = idx < 0 ? 0 : idx;
  const isReady = phase === 'Ready';

  return (
    <Stepper
      active={active}
      size="sm"
      iconSize={30}
      orientation={vertical ? 'vertical' : 'horizontal'}
      completedIcon={<IconCircleCheck size={18} />}
    >
      {PROVISION_STEPS.map((step) => (
        <Stepper.Step
          key={step}
          color={isReady ? 'teal' : phaseMeta(step).color}
          label={phaseMeta(step).label}
          loading={!isReady && step === phase}
        />
      ))}
    </Stepper>
  );
}

export function GenerationHint({ cluster }: { cluster: Cluster }) {
  if (cluster.observed_generation === cluster.generation) return null;
  return (
    <Group gap={6} c="blue">
      <Text size="xs">
        converging generation {cluster.observed_generation} → {cluster.generation}
      </Text>
    </Group>
  );
}
