import type { ClientKey } from './api';

export function clientKeyStatus(key: Pick<ClientKey, 'revoked_at' | 'expires_at'>, now: number): 'Revoked' | 'Expired' | 'Active' {
  if (key.revoked_at) return 'Revoked';
  return key.expires_at > 0 && key.expires_at <= now ? 'Expired' : 'Active';
}
