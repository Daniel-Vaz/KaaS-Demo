import { Button } from '@mantine/core';
import { IconError404 } from '@tabler/icons-react';
import { Link } from 'react-router-dom';
import { EmptyState } from '../components/EmptyState';

export function NotFound() {
  return (
    <EmptyState
      icon={IconError404}
      title="Page not found"
      description="That page doesn't exist in the console."
      action={
        <Button component={Link} to="/" variant="light" mt="sm">
          Back to overview
        </Button>
      }
    />
  );
}
