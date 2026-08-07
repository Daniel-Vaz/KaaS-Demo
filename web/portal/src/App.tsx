import { Routes, Route, Navigate } from 'react-router';
import { Center, Loader } from '@mantine/core';
import { AppLayout } from './components/AppLayout';
import { Overview } from './pages/Overview';
import { Clusters } from './pages/Clusters';
import { CreateCluster } from './pages/CreateCluster';
import { ClusterDetail } from './pages/ClusterDetail';
import { Workloads } from './pages/Workloads';
import { WorkloadDetail } from './pages/WorkloadDetail';
import { Storage } from './pages/Storage';
import { Secrets } from './pages/Secrets';
import { Registry } from './pages/Registry';
import { Networking } from './pages/Networking';
import { Monitoring } from './pages/Monitoring';
import { Security } from './pages/Security';
import { Catalog } from './pages/Catalog';
import { Admin } from './pages/Admin';
import { Profile } from './pages/Profile';
import { Login } from './pages/Login';
import { NotFound } from './pages/NotFound';
import { useAuth } from './lib/auth';

export function App() {
  const { user, isLoading } = useAuth();

  // While resolving the session, hold on a spinner so we don't flash the login screen.
  if (isLoading) {
    return (
      <Center mih="100vh">
        <Loader />
      </Center>
    );
  }

  // Unauthenticated: the login/register gate. No app shell.
  if (!user) return <Login />;

  return (
    <AppLayout>
      <Routes>
        <Route path="/" element={<Overview />} />
        <Route path="/clusters" element={<Clusters />} />
        <Route path="/clusters/new" element={<CreateCluster />} />
        <Route path="/clusters/:id" element={<ClusterDetail />} />
        <Route path="/workloads" element={<Workloads />} />
        <Route path="/workloads/:clusterId/:kind/:namespace/:name" element={<WorkloadDetail />} />
        <Route path="/storage" element={<Storage />} />
        <Route path="/secrets" element={<Secrets />} />
        <Route path="/registry" element={<Registry />} />
        <Route path="/networking" element={<Networking />} />
        <Route path="/monitoring" element={<Monitoring />} />
        <Route path="/security" element={<Security />} />
        <Route path="/catalog" element={<Catalog />} />
        {/* Reached from the account menu rather than the navbar - it's about you, not the fleet. */}
        <Route path="/profile" element={<Profile />} />
        {user.is_admin && <Route path="/admin" element={<Admin />} />}
        <Route path="/404" element={<NotFound />} />
        <Route path="*" element={<Navigate to="/404" replace />} />
      </Routes>
    </AppLayout>
  );
}
