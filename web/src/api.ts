import type { Session } from './types';

const base = import.meta.env.BASE_URL.replace(/\/$/, '');
let csrfToken = '';

export class ApiError extends Error {
  status: number;
  code: string;
  payload: unknown;

  constructor(status: number, code: string, message: string, payload: unknown) {
    super(message);
    this.status = status;
    this.code = code;
    this.payload = payload;
  }
}

export function apiPath(path: string): string {
  return `${base}${path}`;
}

export async function loadSession(): Promise<Session> {
  const session = await request<Session>('/api/v1/session');
  csrfToken = session.csrfToken;
  return session;
}

export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  if (init.method && !['GET', 'HEAD'].includes(init.method.toUpperCase())) {
    headers.set('X-CSRF-Token', csrfToken);
    headers.set('X-QVMC-Client', 'qvmconsole-manager');
  }
  const response = await fetch(apiPath(path), { ...init, headers, credentials: 'same-origin' });
  const contentType = response.headers.get('Content-Type') || '';
  const payload = contentType.includes('application/json') ? await response.json() : await response.text();
  if (!response.ok) {
    const error = typeof payload === 'object' && payload && 'error' in payload
      ? (payload as { error: { code?: string; message?: string } }).error
      : undefined;
    throw new ApiError(response.status, error?.code || 'request_failed', error?.message || `请求失败（HTTP ${response.status}）`, payload);
  }
  return payload as T;
}
