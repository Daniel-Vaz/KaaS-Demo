// Auth context: resolves the current user from GET /auth/me and gates the app. The session lives
// in an HttpOnly cookie the browser sends automatically, so there's no token to hold in JS - we
// just ask the API who we are. A 401 (not logged in / expired) leaves user null, which App.tsx
// renders as the login screen.

import { createContext, useContext, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from './api';
import type { User } from './types';

export const meKey = ['me'] as const;

interface AuthState {
  user: User | null;
  isLoading: boolean;
  setUser: (u: User | null) => void; // called by login/register/logout to update the gate instantly
}

const AuthContext = createContext<AuthState>({ user: null, isLoading: true, setUser: () => {} });

export function AuthProvider({ children }: { children: ReactNode }) {
  const qc = useQueryClient();
  const { data, isLoading, isError, error } = useQuery({
    queryKey: meKey,
    queryFn: api.me,
    retry: (count, err) => !(err instanceof ApiError && err.status === 401) && count < 1,
    staleTime: 60_000,
  });

  // A 401 is the expected "logged out" state, not a hard error.
  const unauthenticated = isError && error instanceof ApiError && error.status === 401;
  const user = data ?? null;

  return (
    <AuthContext.Provider
      value={{
        user: unauthenticated ? null : user,
        isLoading,
        setUser: (u) => qc.setQueryData(meKey, u),
      }}
    >
      {children}
    </AuthContext.Provider>
  );
}

// eslint-disable-next-line react-refresh/only-export-components
export const useAuth = () => useContext(AuthContext);
