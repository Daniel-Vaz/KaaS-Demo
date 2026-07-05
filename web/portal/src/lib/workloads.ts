// Presentation helpers for the Workloads page, shared by the list and detail views. Mirrors the
// kinds and scalability rule in internal/kube.

import type { WorkloadKind } from './types';

export function kindLabel(kind: WorkloadKind): string {
  return {
    deployment: 'Deployment',
    statefulset: 'StatefulSet',
    daemonset: 'DaemonSet',
    job: 'Job',
    cronjob: 'CronJob',
  }[kind];
}

// kindColor gives each kind a stable Mantine color for its Type badge.
export function kindColor(kind: WorkloadKind): string {
  return {
    deployment: 'blue',
    statefulset: 'grape',
    daemonset: 'teal',
    job: 'orange',
    cronjob: 'cyan',
  }[kind];
}

// statusColor maps a workload/pod status string to a Mantine color.
export function statusColor(status: string): string {
  switch (status) {
    case 'Running':
    case 'Complete':
    case 'Completed':
    case 'Scheduled':
      return 'green';
    case 'Progressing':
    case 'Pending':
    case 'ContainerCreating':
      return 'yellow';
    case 'Failed':
    case 'CrashLoopBackOff':
    case 'ImagePullBackOff':
    case 'ErrImagePull':
    case 'Error':
      return 'red';
    default:
      return 'gray'; // Suspended, Scaled to zero, Unknown, …
  }
}

// scalable mirrors WorkloadKind.Scalable server-side: only Deployments and StatefulSets take a
// replica count. The server is the real gate.
export function scalable(kind: WorkloadKind): boolean {
  return kind === 'deployment' || kind === 'statefulset';
}
