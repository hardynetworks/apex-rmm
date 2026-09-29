import { createContext } from 'preact';
import { useContext } from 'preact/hooks';
import type { User } from './api';
import { can } from './api';

export const UserContext = createContext<User | null>(null);

export function useUser(): User {
  return useContext(UserContext)!;
}

export function useCan(role: 'admin' | 'technician' | 'viewer'): boolean {
  return can(useContext(UserContext), role);
}
