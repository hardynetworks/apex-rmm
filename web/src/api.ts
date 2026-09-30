// Thin fetch wrapper for the Apex RMM API.

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T = any>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch('/api' + path, {
    method,
    credentials: 'same-origin',
    headers: body !== undefined ? { 'Content-Type': 'application/json', 'X-Apex-CSRF': '1' } : { 'X-Apex-CSRF': '1' },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401 && !path.startsWith('/auth/')) {
    const ret = encodeURIComponent(location.pathname + location.search);
    location.href = '/login?return=' + ret;
    throw new ApiError(401, 'not signed in');
  }
  const text = await res.text();
  let data: any = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = text;
  }
  if (!res.ok) throw new ApiError(res.status, (data && data.error) || res.statusText);
  return data as T;
}

export const api = {
  get: <T = any>(p: string) => request<T>('GET', p),
  post: <T = any>(p: string, b: unknown = {}) => request<T>('POST', p, b),
  put: <T = any>(p: string, b: unknown) => request<T>('PUT', p, b),
  patch: <T = any>(p: string, b: unknown) => request<T>('PATCH', p, b),
  del: <T = any>(p: string) => request<T>('DELETE', p),
};

export function wsUrl(path: string): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${location.host}/api${path}`;
}

export type User = { id: string; email: string; name: string; username: string; role: 'admin' | 'technician' | 'viewer'; local: boolean };

export function can(user: User | null, role: 'admin' | 'technician' | 'viewer'): boolean {
  const rank = { viewer: 1, technician: 2, admin: 3 } as const;
  return !!user && rank[user.role] >= rank[role];
}
