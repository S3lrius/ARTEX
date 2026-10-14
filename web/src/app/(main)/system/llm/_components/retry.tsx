"use client";

// Shared LLM retry settings: attempts and interval for each of the five layers.
//
// Inner to outer: connect (SDK) → empty response (SDK) → same-provider safe window → pool circuit breaker → intent rerun.
// The first three follow the endpoint, so each model profile can override the global default. The last two are process-wide and exist once.
//
// Every input uses the same "empty = not set" meaning as the backend db.RetryRule:
//   attempts   empty/0 = built-in default | -1 = turn this layer off | >0 = use this count
//   interval   empty/0 = this layer's exponential backoff | >0 = this fixed interval in milliseconds

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
  /** Where this retry happens and who runs it */
  where: string;
  /** Which errors reach this layer, down to status codes */
  trigger: string;
  /** Errors that look similar but do not use this layer, so a no-op is not mistaken for a bug */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** Default attempt count shown in the placeholder when the field is empty */
  defAttempts: number;
  /** Default interval strategy shown in the placeholder when the field is empty */
  defInterval: string;
  /** What attempts = -1 means */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "Connect retry",
    where: "SDK · before a 200",
    trigger:
      "Cannot connect, or no 200 yet: connection reset, read/write timeout, DNS failure, and other network errors, plus HTTP 408, 429, 500, 502, 503, and 504.",
    skips: "Other status codes (400, 401, 403, 404, 413, 422, and similar) are definitive rejections. Retrying them fails the same way, so they are raised immediately.",
    desc: "Resends the same request. Once the stream has started (a 200 was received), a mid-stream drop is not this layer's job.",
    attemptsLabel: "Attempts",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = do not retry; raise the failure immediately",
  },
  empty: {
    title: "Empty-response retry",
    where: "SDK · openai format only",
    trigger:
      "HTTP 200 and a normal stop finish_reason, but the response has no content blocks. Empty gateway frames, dropped thinking fields, and sampling glitches all look like this.",
    skips: "A response cut off by max_tokens does not count. Raise the output limit; resending only hits the same cap.",
    desc: "Resends the whole prompt, which is expensive on a long context, so keep the count small.",
    attemptsLabel: "Attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = return an empty response as-is",
  },
  stream: {
    title: "Same-provider safe-window retry",
    where: "This app · before any output is delivered",
    trigger:
      "The stream was established (a 200 arrived) and then failed: the connection dropped, the provider is overloaded, or the stream emitted a 429 or 5xx — and not a single token has been handed to the caller.",
    skips:
      "Quota exhausted (402 / insufficient_quota, handled by switching profiles), context too long (413 / context length, handled by compression), and definitive 400 / 401 / 403 / 404 / 422 rejections are not retried.",
    desc: "Replays the same request on the same profile. Nothing has been delivered yet, so the replay does not repeat model output or tool execution.",
    attemptsLabel: "Attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s exponential (capped at 4s)",
    offHint: "-1 = hand a broken stream to the outer intent rerun",
  },
  breaker: {
    title: "Pool circuit breaker",
    where: "This app · process-wide, one copy",
    trigger:
      "Transient failures (429, 5xx, network errors) trip the breaker once they reach the threshold in a row. Definitive failures — out of credit (402), invalid key (401 / 403), unknown model (404) — trip it on the first hit.",
    skips: "One success resets the count, so a profile that fails occasionally is not slowly accumulated into a trip.",
    desc: "A tripped profile enters cooldown and the pool skips it until cooldown ends. The state is stored and survives a restart.",
    attemptsLabel: "Failures before trip",
    defAttempts: 3,
    defInterval: "1min→5min→30min steps",
    offHint: "-1 = never trip on transient failures (definitive failures still trip)",
  },
  intent: {
    title: "Intent rerun",
    where: "This app · process-wide, one copy",
    trigger:
      "The inner layers did not recover: the worker ended with model_error because retries were exhausted, or the stream dropped after output had already started (replaying then is unsafe, so the whole intent starts over).",
    skips: "Quota exhaustion is already handled by switching profiles and is not rerun here. A paused, stopped, or wrapping-up task yields immediately and does not spend the backoff.",
    desc: "Runs the whole intent again from the start. It is the outermost layer, so one rerun multiplies the attempt counts inside it.",
    attemptsLabel: "Reruns",
    defAttempts: 2,
    defInterval: "fixed 3s",
    offHint: "-1 = do not rerun; mark the intent blocked",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** Readable milliseconds, shown beside the input so you do not have to count zeros. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled number input: empty string ↔ 0. Intermediate text ("-", "1e") stays local and does not notify the parent. */
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
  // Follow a wholesale parent update (policy loaded, profile switched). Typing does not
  // land here, because value already equals the parsed local text.
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

/** The two controls for one retry layer. idPrefix keeps htmlFor unique when the fields appear more than once on a page. */
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
  /** true = the compact profile-drawer version: drop the long notes and keep only which errors reach this layer */
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
        {/* Which errors reach this layer, down to status codes. A control that seems to do nothing usually means the error never lands here. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">Triggers</span>: {meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">Skips</span>: {meta.skips}
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
      {!compact && <p className="text-muted-foreground text-xs">Empty = default. {meta.offHint}.</p>}
    </div>
  );
}

/** The three per-endpoint overrides inside the model-profile drawer. */
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
          Applies only to this profile and overrides the global defaults on Retry and backoff. Empty = follow the global value. Attempts of -1 turns that layer off.
          A filled interval replaces exponential backoff with a fixed delay. The breaker and intent rerun are process-wide and can only be changed on the global page.
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

/** The Retry and backoff tab: global defaults for all five layers. */
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
      toast.error(`Couldn't load the retry policy: ${(e as Error).message}`);
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
      // The backend clamps out-of-range values and returns them. Refresh from that response so the screen matches what was stored.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("Saved. It applies immediately (the call already in flight still uses the old values)");
    } catch (e) {
      toast.error(`Couldn't save: ${(e as Error).message}`);
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
        A failed model call walks five retry layers, inner to outer:
        <span className="text-foreground"> connect → empty response → same-provider safe window → pool circuit breaker → intent rerun</span>
        . An outer layer runs only after the inner one is exhausted, so the counts
        <span className="text-foreground"> multiply</span>
        . Maxing every layer can burn dozens of requests on one glitch.
        Leaving everything empty keeps today's defaults, the same behavior as before this page existed. The first three layers can be overridden on each model profile.
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
          Restore all defaults
        </Button>
      </div>
    </div>
  );
}
