import assert from 'node:assert/strict';
import test from 'node:test';
import { clientKeyStatus } from '../src/client-key-state.ts';

test('client key expires at the exact expiry timestamp', () => {
  assert.equal(clientKeyStatus({ expires_at: 500, revoked_at: 0 }, 500), 'Expired');
});

test('future and zero expiry timestamps remain active', () => {
  assert.equal(clientKeyStatus({ expires_at: 501, revoked_at: 0 }, 500), 'Active');
  assert.equal(clientKeyStatus({ expires_at: 0, revoked_at: 0 }, 500), 'Active');
});

test('revocation takes precedence over expiry', () => {
  assert.equal(clientKeyStatus({ expires_at: 400, revoked_at: 123 }, 500), 'Revoked');
});
