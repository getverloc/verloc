import { useState, type Dispatch, type SetStateAction } from 'react';
import { upsertAdminProvider, type AdminProvider } from '../../api';
import { parseKeyLine, type EditorActions } from './shared';

type ProviderDraft = Partial<Omit<AdminProvider, 'cooldown_seconds' | 'request_timeout_seconds' | 'max_tokens' | 'max_in_flight' | 'keep_warm_seconds'>> & {
  name: string; keys_text?: string; append_key?: string;
  cooldown_seconds?: string; request_timeout_seconds?: string; max_tokens?: string;
  max_in_flight?: string; keep_warm_seconds?: string;
};

export type ProviderEditorState = {
  editProvider: ProviderDraft | null;
  setEditProvider: Dispatch<SetStateAction<ProviderDraft | null>>;
  originalProviderName: string;
  saveProvider: () => void;
  startNew: () => void;
  edit: (provider: AdminProvider) => void;
};

export function useProviderEditor({ providers, run, setErr, setConfirm }: EditorActions & { providers: AdminProvider[] }): ProviderEditorState {
  const [editProvider, setEditProvider] = useState<ProviderDraft | null>(null);
  const [originalProviderName, setOriginalProviderName] = useState('');
  const doSaveProvider = (body: Record<string, unknown>) => {
    void run(async () => {
      await upsertAdminProvider(body);
      setEditProvider(null);
    }, 'Provider saved · live apply');
  };

  const saveProvider = () => {
    if (!editProvider?.name || !editProvider.base_url) {
      setErr('name and base_url required');
      return;
    }
    const maxInFlight = editProvider.max_in_flight?.trim() ? Number(editProvider.max_in_flight) : 0;
    if (!Number.isInteger(maxInFlight) || maxInFlight < 0 || maxInFlight > 4096) {
      setErr('max_in_flight must be an integer between 0 and 4096');
      return;
    }
    const keepWarmSeconds = editProvider.keep_warm_seconds?.trim() ? Number(editProvider.keep_warm_seconds) : 0;
    if (!Number.isInteger(keepWarmSeconds) || keepWarmSeconds < 0 || keepWarmSeconds > 300) {
      setErr('keep_warm_seconds must be an integer between 0 and 300');
      return;
    }
    const existing = providers.find((p) => p.name === editProvider.name.trim());
    const connChanged = existing && (
      existing.base_url !== editProvider.base_url.trim() ||
      existing.auth_header !== (editProvider.auth_header || '') ||
      existing.auth_header_format !== (editProvider.auth_header_format || '') ||
      existing.no_auth !== !!editProvider.no_auth ||
      (existing.api_format || 'openai') !== (editProvider.api_format || 'openai')
    );
    const body: Record<string, unknown> = {
      name: editProvider.name.trim(),
      base_url: editProvider.base_url.trim(),
      api_format: editProvider.api_format || 'openai',
      auth_header: editProvider.auth_header || '',
      auth_header_format: editProvider.auth_header_format || '',
      disabled: !!editProvider.disabled,
      cooldown_seconds: Number(editProvider.cooldown_seconds) || 0,
      request_timeout_seconds: Number(editProvider.request_timeout_seconds) || 0,
      no_auth: !!editProvider.no_auth,
      synthesize_cli_headers: !!editProvider.synthesize_cli_headers,
      allow_private_upstreams: !!editProvider.allow_private_upstreams,
      strip_reasoning_content: editProvider.strip_reasoning_content ?? null,
      max_tokens: Number(editProvider.max_tokens) || 0,
      max_in_flight: maxInFlight,
      keep_warm_seconds: keepWarmSeconds,
    };
    if ((!existing || !originalProviderName) && editProvider.keys_text !== undefined) {
      body.keys = editProvider.keys_text
        .split('\n')
        .map((s) => s.trim())
        .filter(Boolean)
        .map((line) => {
          const parsed = parseKeyLine(line);
          return parsed.account_id
            ? { account_id: parsed.account_id, api_key: parsed.api_key }
            : parsed.api_key;
        });
    }
    if (editProvider.append_key?.trim()) {
      const parsed = parseKeyLine(editProvider.append_key.trim());
      body.append_keys = [parsed.account_id
        ? { account_id: parsed.account_id, api_key: parsed.api_key }
        : parsed.api_key];
    }
    if ((connChanged || (existing && !originalProviderName))) {
      setConfirm({
        title: `Overwrite ${existing?.name}?`,
        body: `Existing model routes will use these connection settings${!originalProviderName ? ' and new credentials replace the stored ones' : ''}.`,
        confirmLabel: 'Overwrite provider',
        run: () => doSaveProvider(body),
      });
      return;
    }
    doSaveProvider(body);
  };

  const startNew = () => {
    setOriginalProviderName('');
    setEditProvider({
      name: '', base_url: '', keys_text: '', no_auth: false,
      synthesize_cli_headers: false, max_tokens: '', cooldown_seconds: '30',
      max_in_flight: '', keep_warm_seconds: '',
    });
  };
  const edit = (provider: AdminProvider) => {
    setOriginalProviderName(provider.name);
    setEditProvider({
      ...provider, append_key: '',
      cooldown_seconds: provider.cooldown_seconds ? String(provider.cooldown_seconds) : '',
      request_timeout_seconds: provider.request_timeout_seconds ? String(provider.request_timeout_seconds) : '',
      max_tokens: provider.max_tokens ? String(provider.max_tokens) : '',
      max_in_flight: provider.max_in_flight ? String(provider.max_in_flight) : '',
      keep_warm_seconds: provider.keep_warm_seconds ? String(provider.keep_warm_seconds) : '',
    });
  };
  return { editProvider, setEditProvider, originalProviderName, saveProvider, startNew, edit };
}
