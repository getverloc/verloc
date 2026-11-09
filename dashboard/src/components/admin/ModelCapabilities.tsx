import type { AdminModel } from '../../api';
import { Field, FieldLabel } from '../ui/field';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../ui/select';
import { Textarea } from '../ui/textarea';

export type ModelCapabilitiesDraft = Pick<AdminModel, 'attachment' | 'reasoning' | 'temperature' | 'modalities'> & {
  input_modalities_text?: string;
  output_modalities_text?: string;
};

type ModelCapabilitiesProps = {
  model: ModelCapabilitiesDraft;
  onChange: (patch: Partial<ModelCapabilitiesDraft>) => void;
};

export function ModelCapabilities({ model, onChange }: ModelCapabilitiesProps) {
  return (
    <div className="min-w-0 space-y-4 border border-edge-700 bg-edge-950 p-4">
      <p className="text-[10px] uppercase tracking-[0.14em] text-chart-3">Capabilities</p>
      <p id="em-capabilities-help" className="text-[11px] leading-relaxed text-muted-foreground">
        Unset fields inherit catalog metadata; leave an unselected field untouched to preserve unknown metadata.
        Choosing Supported or Not supported saves an explicit value. Saved values cannot be reset to unknown here.
      </p>
      <div className="grid min-w-0 grid-cols-1 gap-3 sm:grid-cols-3">
        {([['attachment', 'Attachments'], ['reasoning', 'Reasoning'], ['temperature', 'Temperature']] as const).map(([field, label]) => (
          <Field key={field} className="min-w-0">
            <FieldLabel htmlFor={`em-${field}`}>{label}</FieldLabel>
            <Select
              value={model[field] === undefined ? '' : String(model[field])}
              onValueChange={(value) => {
                if (value === 'true' || value === 'false') onChange({ [field]: value === 'true' });
              }}
            >
              <SelectTrigger id={`em-${field}`} aria-describedby="em-capabilities-help" className="w-full min-w-0">
                <SelectValue placeholder="Inherited / unknown">
                  {model[field] === undefined ? 'Inherited / unknown' : model[field] ? 'Supported' : 'Not supported'}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="true">Supported</SelectItem>
                <SelectItem value="false">Not supported</SelectItem>
              </SelectContent>
            </Select>
          </Field>
        ))}
      </div>
      <p id="em-modalities-help" className="text-[11px] leading-relaxed text-muted-foreground">
        One modality per line, for example text, image, or audio. Untouched blank fields remain unknown.
        Clearing an edited list saves an empty list, not unknown. Other modality metadata is preserved.
      </p>
      <div className="grid min-w-0 grid-cols-1 gap-3 sm:grid-cols-2">
        {(['input', 'output'] as const).map((direction) => {
          const textKey = direction === 'input' ? 'input_modalities_text' : 'output_modalities_text';
          return (
            <Field key={direction} className="min-w-0">
              <FieldLabel htmlFor={`em-modalities-${direction}`}>{direction === 'input' ? 'Input modalities' : 'Output modalities'}</FieldLabel>
              <Textarea
                id={`em-modalities-${direction}`}
                value={model[textKey] ?? model.modalities?.[direction]?.join('\n') ?? ''}
                onChange={(e) => onChange({ [textKey]: e.target.value })}
                rows={3}
                aria-describedby="em-modalities-help"
                placeholder={model[textKey] !== undefined || model.modalities?.[direction] !== undefined ? 'No modalities' : 'Inherited / unknown'}
                className="min-w-0 font-mono"
              />
            </Field>
          );
        })}
      </div>
    </div>
  );
}
