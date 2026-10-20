"use client";

// Shared pieces for LLM retry configuration: each of the five retry layers has its
// own "attempts + interval".
//
// The five layers, from innermost to outermost: connect (SDK) -> empty response (SDK)
// -> same-provider safety window -> pool circuit breaker -> intent re-run.
// The first three follow the endpoint, so each model profile can override the global
// default; the last two are process-level, with only one global copy.
//
// All inputs follow the same "blank = unconfigured" semantics, matching the backend's
// db.RetryRule:
//   attempts   blank/0 = use the built-in default | -1 = disable this layer's retry | >0 = use this count
//   interval   blank/0 = use this layer's own exponential backoff | >0 = use this fixed interval in ms instead

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** Where this retry layer happens and who executes it */
  where: string;
  /** What kind of error reaches this layer — specific status codes, so no one has to guess */
  trigger: string;
  /** Errors that look similar but do [NOT] go through this layer, so a configured knob that has no effect isn't mistaken for a bug */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** Default value when attempts is left blank, used as the placeholder */
  defAttempts: number;
  /** Default strategy when interval is left blank, used as the placeholder */
  defInterval: string;
  /** What setting attempts to -1 means */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "Connect Retry",
    where: "SDK · before a 200 is received",
    trigger:
      "Can't connect or haven't received a 200 yet: network-layer errors like connection reset / read-write timeout / DNS failure, plus HTTP 408, 429, 500, 502, 503, 504.",
    skips: "Other status codes (400 / 401 / 403 / 404 / 413 / 422, etc.) are deterministic rejections that would fail again on retry, so they're thrown immediately.",
    desc: "Resends the same request as-is. Once the stream has started (a 200 was already received), a mid-stream disconnect is no longer this layer's concern.",
    attemptsLabel: "Retry attempts",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = never retry, throw immediately on failure",
  },
  empty: {
    title: "Empty Response Retry",
    where: "SDK · openai format only",
    trigger:
      "HTTP 200, finish_reason is a normal stop, but the entire response has zero content blocks — gateway empty frames, dropped thinking-field frames, and sampling hiccups all look like this.",
    skips: "Doesn't count content missing due to max_tokens truncation (that's fixed by raising the output cap — retrying would just hit the same wall again).",
    desc: "Resends the whole prompt, so it's relatively expensive on long context — keep the attempt count modest.",
    attemptsLabel: "Retry attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = hand back an empty response as-is",
  },
  stream: {
    title: "Same-Provider Safety Window Retry",
    where: "This project · before any output has been delivered",
    trigger:
      "Something goes wrong after the stream has already started (a 200 was received): the connection drops mid-stream, the provider reports overloaded, or a 429 / 5xx error event appears in the stream — and not a single token has been handed to the caller yet.",
    skips:
      "Not retried: quota exhausted (402 / insufficient_quota, handled by the pool switching profiles), context too long (413 / context length, handled by compaction), and deterministic rejections 400 / 401 / 403 / 404 / 422.",
    desc: "Replays the same request on the same profile. Since no output has been delivered yet, replaying never duplicates model output or tool execution.",
    attemptsLabel: "Retry attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s exponential (capped at 4s)",
    offHint: "-1 = hand a dropped stream straight to the outer intent re-run",
  },
  breaker: {
    title: "Pool Circuit Breaker",
    where: "This project · process-level, one global copy",
    trigger:
      "Trips when transient failures (429, 5xx, network errors) accumulate to the threshold; deterministic failures like insufficient balance (402), invalid key (401 / 403), or model not found (404) ignore the threshold and trip on the very first occurrence.",
    skips: "A single success resets the count to zero, so a profile that occasionally hiccups won't slowly accumulate toward tripping.",
    desc: "Once tripped, the profile enters cooldown, during which the pool skips it entirely. State is persisted, so it survives restarts.",
    attemptsLabel: "Consecutive failures before tripping",
    defAttempts: 3,
    defInterval: "1min→5min→30min staircase",
    offHint: "-1 = transient failures never trip it (deterministic failures still trip it)",
  },
  intent: {
    title: "Intent Re-run",
    where: "This project · process-level, one global copy",
    trigger:
      "None of the earlier layers caught it: the worker ends with model_error — either the inner retries are all exhausted, or the stream broke after it had already started delivering output (replaying isn't safe at that point, so the whole thing has to run again from scratch).",
    skips: "Quota exhaustion is already handled by the pool switching profiles and isn't re-run here; an intent yields immediately without consuming backoff time when the task is paused / terminated / entering wrap-up.",
    desc: "Re-runs the entire intent from scratch. It's the outermost layer, so one re-run multiplies the attempt counts of all the inner layers.",
    attemptsLabel: "Re-run attempts",
    defAttempts: 2,
    defInterval: "fixed 3s",
    offHint: "-1 = never re-run, mark the intent as blocked immediately",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** Human-readable milliseconds, shown next to the input just so no one has to count zeros. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled number input: empty string <-> 0, intermediate states ("-", "1e") stay local as-is without bothering the parent. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // Stay in sync when the parent swaps in a whole new value (policy loaded, profile
  // switched); this never runs while typing, because at that point value already
  // equals the parsed result of the local text.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** The two knobs for one retry layer. idPrefix keeps the label's htmlFor unique when this appears multiple times on the same page. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = the compact version used in the profile drawer: drops the expanded explanation, keeping only the "what errors reach this layer" line */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* Which errors reach this layer, down to the status code — if a knob seems to have no effect, the error probably isn't landing in this layer at all. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">Triggers on</span>: {meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">Doesn't apply to</span>: {meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`Default ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            Interval ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="Default backoff"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `Fixed ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">Blank = use the default; {meta.offHint}.</p>}
    </div>
  );
}

/** The three overridable layers in the model profile drawer (the three that follow the endpoint). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">Retry overrides</Label>
        <p className="text-muted-foreground text-xs">
          Applies only to this profile, overriding the global defaults on the "Retry & Backoff"
          page. Blank in a field = follow global; attempts = -1 disables that layer's retry;
          setting an interval replaces exponential backoff with that fixed interval. The circuit
          breaker and intent re-run are process-level and can only be tuned on the global page.
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** The "Retry & Backoff" tab: global defaults for the five layers. */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`Failed to load retry policy: ${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // The backend clamps out-of-range values back into the valid range and returns
      // them; refresh with that returned value directly, so what's shown is what's saved.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("Saved, effective immediately (calls already in progress still use the old parameters)");
    } catch (e) {
      toast.error(`Save failed: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> Loading retry policy…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        A failed model call passes through five retry layers in order, innermost to outermost:
        <span className="text-foreground"> connect → empty response → same-provider safety window → pool circuit breaker → intent re-run</span>
        . Outer layers only kick in once inner ones are exhausted, so attempt counts
        <span className="text-foreground"> multiply</span>
        — max out every layer and a single blip can burn through dozens of requests.
        Leaving everything blank is the current default, identical to behavior before this page existed. The first three layers can be overridden individually on each model profile.
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          Save
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          Reset all to defaults
        </Button>
      </div>
    </div>
  );
}
