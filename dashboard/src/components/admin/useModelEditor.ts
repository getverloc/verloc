import { useState, type Dispatch, type SetStateAction } from 'react';
import { upsertAdminModel, type AdminModel, type AdminProvider } from '../../api';
import { hasPriceField, type EditorActions } from './shared';
import type { ModelCapabilitiesDraft } from './ModelCapabilities';

type ModelDraft = Partial<Omit<AdminModel, 'context_window' | 'max_tokens' | 'pricing_input' | 'pricing_output' | 'pricing_cache' | 'pricing_image'>> & ModelCapabilitiesDraft & {
  id: string; context_window?: string; max_tokens?: string;
  pricing_input?: string; pricing_output?: string; pricing_cache?: string; pricing_image?: string;
  tags_text: string;
};

export type ModelEditorState = {
  editModel: ModelDraft | null;
  setEditModel: Dispatch<SetStateAction<ModelDraft | null>>;
  originalModelID: string;
  saveModel: () => void;
  startNew: () => void;
  edit: (model: AdminModel) => void;
};

export function useModelEditor({ providers, models, run, setErr, setConfirm }: EditorActions & { providers: AdminProvider[]; models: AdminModel[] }): ModelEditorState {
  const [editModel, setEditModel] = useState<ModelDraft | null>(null);
  const [originalModelID, setOriginalModelID] = useState('');
  const doSaveModel = (body: Record<string, unknown>, existingID?: string) => {
    void run(async () => {
      await upsertAdminModel(body, existingID);
      setEditModel(null);
    }, 'Model saved · live apply');
  };

  const saveModel = () => {
    if (!editModel?.id || !editModel.backend || !editModel.upstream) {
      setErr('id, backend, and upstream required');
      return;
    }
    const existing = models.find((model) => model.id === editModel.id.trim());
    for (const value of [editModel.pricing_input, editModel.pricing_output, editModel.pricing_cache, editModel.pricing_image]) {
      if (value?.trim() && (!Number.isFinite(Number(value)) || Number(value) < 0)) {
        setErr('Prices must be finite, non-negative USD values');
        return;
      }
    }
    const routingChanged = existing && (existing.backend !== editModel.backend || existing.upstream !== editModel.upstream || JSON.stringify(existing.routes || []) !== JSON.stringify(editModel.routes || []));
    const body = {
      id: editModel.id.trim(),
      backend: editModel.backend.trim(),
      disabled: !!editModel.disabled,
      routes: editModel.routes || [],
      overwrite: !!existing,
      upstream: editModel.upstream.trim(),
      name: editModel.name || editModel.id,
      description: editModel.description || '',
      owned_by: editModel.owned_by || '',
      context_window: Number(editModel.context_window) || 0,
      max_tokens: Number(editModel.max_tokens) || 0,
      tags: editModel.tags_text.split(',').map((tag) => tag.trim()).filter(Boolean),
      attachment: editModel.attachment,
      reasoning: editModel.reasoning,
      temperature: editModel.temperature,
      modalities: editModel.modalities || editModel.input_modalities_text !== undefined || editModel.output_modalities_text !== undefined ? {
        ...editModel.modalities,
        ...(editModel.input_modalities_text !== undefined ? { input: editModel.input_modalities_text.split('\n').map((value) => value.trim()).filter(Boolean) } : {}),
        ...(editModel.output_modalities_text !== undefined ? { output: editModel.output_modalities_text.split('\n').map((value) => value.trim()).filter(Boolean) } : {}),
      } : undefined,
      ...(editModel.pricing_input?.trim() ? { pricing_input: Number(editModel.pricing_input) } : {}),
      ...(editModel.pricing_output?.trim() ? { pricing_output: Number(editModel.pricing_output) } : {}),
      ...(editModel.pricing_cache?.trim() ? { pricing_cache: Number(editModel.pricing_cache) } : {}),
      ...(editModel.pricing_image?.trim() ? { pricing_image: Number(editModel.pricing_image) } : { clear_image_price: true }),
      image_prices: editModel.image_prices || {},
      ...(editModel.operations !== undefined ? { operations: editModel.operations } : {}),
    };
    if (routingChanged || (existing && !originalModelID)) {
      setConfirm({
        title: `Overwrite model ${editModel.id.trim()}?`,
        body: 'Existing applications using this ID will use the reviewed routing and metadata.',
        confirmLabel: 'Overwrite model',
        run: () => doSaveModel(body, originalModelID || undefined),
      });
      return;
    }
    doSaveModel(body, originalModelID || undefined);
  };

  const startNew = () => {
    setOriginalModelID('');
    setEditModel({
      id: '', backend: providers[0]?.name || '', upstream: '',
      name: '', context_window: '', max_tokens: '', tags_text: '',
      pricing_input: '', pricing_output: '', pricing_cache: '', pricing_image: '',
    });
  };
  const edit = (model: AdminModel) => {
    setOriginalModelID(model.id);
    setEditModel({
      ...model, tags_text: (model.tags || []).join(', '),
      context_window: model.context_window ? String(model.context_window) : '',
      max_tokens: model.max_tokens ? String(model.max_tokens) : '',
      pricing_input: hasPriceField(model, 'input') ? String(model.pricing_input) : '',
      pricing_output: hasPriceField(model, 'output') ? String(model.pricing_output) : '',
      pricing_cache: hasPriceField(model, 'cache_read') ? String(model.pricing_cache) : '',
      pricing_image: model.pricing_image != null ? String(model.pricing_image) : '',
    });
  };
  return { editModel, setEditModel, originalModelID, saveModel, startNew, edit };
}
