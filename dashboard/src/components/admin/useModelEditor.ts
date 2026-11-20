import { useState, type Dispatch, type SetStateAction } from 'react';
import { upsertAdminModel, type AdminModel, type AdminProvider, type RoutePolicy } from '../../api';
import { hasPriceField, type EditorActions } from './shared';
import type { ModelCapabilitiesDraft } from './ModelCapabilities';

type ModelDraft = Partial<Omit<AdminModel, 'context_window' | 'max_tokens' | 'pricing_input' | 'pricing_output' | 'pricing_cache' | 'pricing_image'>> & ModelCapabilitiesDraft & {
  id: string; context_window?: string; max_tokens?: string;
  pricing_input?: string; pricing_output?: string; pricing_cache?: string; pricing_image?: string;
  routing_strategy?: 'ordered' | 'latency' | 'weighted';
  routing_primary_weight?: string;
  routing_hedge_delay_ms?: string;
  routing_max_hedges?: string;
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

function normalizedPolicy(p?: RoutePolicy): { strategy: string; primary_weight: number; hedge_delay_ms: number; max_hedges: number } {
  const strategy = p?.strategy || 'ordered';
  return {
    strategy,
    primary_weight: strategy === 'weighted' ? (p?.primary_weight || 0) : 0,
    hedge_delay_ms: p?.hedge_delay_ms || 0,
    max_hedges: p?.max_hedges || 0,
  };
}

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
    const strategy = editModel.routing_strategy || 'ordered';
    const primaryWeight = editModel.routing_primary_weight?.trim() ? Number(editModel.routing_primary_weight) : 0;
    if (!Number.isInteger(primaryWeight) || primaryWeight < 0 || primaryWeight > 1000) {
      setErr('Primary weight must be an integer between 0 and 1000');
      return;
    }
    const hedgeDelayMs = editModel.routing_hedge_delay_ms?.trim() ? Number(editModel.routing_hedge_delay_ms) : 0;
    if (!Number.isInteger(hedgeDelayMs) || hedgeDelayMs < 0 || hedgeDelayMs > 10000) {
      setErr('Hedge delay must be an integer between 0 and 10000 ms');
      return;
    }
    const maxHedges = editModel.routing_max_hedges?.trim() ? Number(editModel.routing_max_hedges) : 0;
    if (!Number.isInteger(maxHedges) || maxHedges < 0 || maxHedges > 2) {
      setErr('Max hedges must be between 0 and 2');
      return;
    }
    const cleanRoutes = (editModel.routes || []).map((r) => {
      const weight = r.weight ? Number(r.weight) : 0;
      return { backend: r.backend, upstream: r.upstream, ...(weight > 0 ? { weight } : {}) };
    });
    for (const r of cleanRoutes) {
      const w = r.weight ?? 0;
      if (!Number.isInteger(w) || w < 0 || w > 1000) {
        setErr('Fallback route weight must be an integer between 0 and 1000');
        return;
      }
    }
    const routingPayload: RoutePolicy = {
      strategy,
      primary_weight: strategy === 'weighted' ? primaryWeight : 0,
      hedge_delay_ms: hedgeDelayMs,
      max_hedges: hedgeDelayMs > 0 ? maxHedges : 0,
    };
    const routingChanged = existing && (
      existing.backend !== editModel.backend ||
      existing.upstream !== editModel.upstream ||
      JSON.stringify(existing.routes || []) !== JSON.stringify(cleanRoutes) ||
      JSON.stringify(normalizedPolicy(existing.routing)) !== JSON.stringify(normalizedPolicy(routingPayload))
    );
    const body = {
      id: editModel.id.trim(),
      backend: editModel.backend.trim(),
      disabled: !!editModel.disabled,
      routes: cleanRoutes,
      routing: routingPayload,
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
      routing_strategy: 'ordered', routing_primary_weight: '', routing_hedge_delay_ms: '', routing_max_hedges: '',
    });
  };
  const edit = (model: AdminModel) => {
    setOriginalModelID(model.id);
    const strat = model.routing?.strategy;
    setEditModel({
      ...model, tags_text: (model.tags || []).join(', '),
      context_window: model.context_window ? String(model.context_window) : '',
      max_tokens: model.max_tokens ? String(model.max_tokens) : '',
      pricing_input: hasPriceField(model, 'input') ? String(model.pricing_input) : '',
      pricing_output: hasPriceField(model, 'output') ? String(model.pricing_output) : '',
      pricing_cache: hasPriceField(model, 'cache_read') ? String(model.pricing_cache) : '',
      pricing_image: model.pricing_image != null ? String(model.pricing_image) : '',
      routing_strategy: strat === 'latency' || strat === 'weighted' ? strat : 'ordered',
      routing_primary_weight: model.routing?.primary_weight ? String(model.routing.primary_weight) : '',
      routing_hedge_delay_ms: model.routing?.hedge_delay_ms ? String(model.routing.hedge_delay_ms) : '',
      routing_max_hedges: model.routing?.max_hedges ? String(model.routing.max_hedges) : '',
    });
  };
  return { editModel, setEditModel, originalModelID, saveModel, startNew, edit };
}
