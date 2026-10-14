"use client";

import { CheckIcon } from "lucide-react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import type { NotificationFilter } from "@/lib/types";

// asText / inputType are local helpers tied to control rendering, so they stay out of channel-fields.
import { type FieldDef, type FieldKind, SEVERITY_OPTIONS } from "./channel-fields";

// asText turns any config value into a string an input can display.
// Config comes from JSON, so values may be string / number / boolean / array / null.
// This only cares whether the value can go in a text box; buildConfig does the serialization.
function asText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === null || v === undefined) return "";
  return String(v);
}

// inputType maps a field kind to an input type attribute.
function inputType(kind: FieldKind): "text" | "password" | "number" {
  if (kind === "password") return "password";
  if (kind === "number") return "number";
  return "text";
}

// ConfigField renders the control for a field definition.
//
// Masked fields are the one special case: the input does not show the mask
// itself, only a "Saved" hint. The rule on screen is simple — text in the box
// is what the user typed, and an empty box is an empty value. Putting
// "__masked__:…abc123" in the input looks like placeholder text the user
// should delete, which makes it easy to wipe a credential by accident.
export function ConfigField({
  def,
  value,
  isSecret,
  onChange,
}: {
  def: FieldDef;
  value: unknown;
  isSecret: boolean;
  onChange: (v: unknown) => void;
}) {
  const id = `n-cfg-${def.key}`;
  const raw = asText(value);
  // Mask echoed by the backend, shaped like "__masked__:…abc123". The tail is a recognizable fragment of the original value.
  const masked = isSecret && raw.startsWith("__masked__");
  const maskedTail = masked ? (raw.split("…")[1] ?? "") : "";

  if (def.kind === "switch") {
    return (
      <div className="flex items-center gap-2 text-sm">
        <Switch checked={value === true} onCheckedChange={onChange} aria-label={def.label} />
        {def.label}
        {def.help && <span className="text-muted-foreground">（{def.help}）</span>}
      </div>
    );
  }

  if (def.kind === "select") {
    return (
      <div className="grid gap-2">
        <Label>{def.label}</Label>
        <Select value={raw || def.options?.[0]?.value} onValueChange={onChange}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(def.options ?? []).map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    );
  }

  // Dispatch the control by field kind. An if chain is clearer than nested ternaries
  // once four controls are involved.
  function control() {
    if (def.kind === "textarea" || def.kind === "kv") {
      return (
        <Textarea
          id={id}
          className="font-mono"
          placeholder={def.placeholder}
          value={masked ? "" : raw}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    }
    if (def.kind === "list") {
      return (
        <Input
          id={id}
          value={Array.isArray(value) ? (value as string[]).join(", ") : raw}
          onChange={(e) => onChange(e.target.value)}
          placeholder={def.placeholder}
        />
      );
    }
    return (
      <Input
        id={id}
        className={def.kind === "text" ? "font-mono" : ""}
        type={inputType(def.kind)}
        placeholder={def.placeholder}
        value={masked ? "" : raw}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  const hint = masked ? (
    <p className="text-muted-foreground flex items-center gap-1 text-xs">
      <CheckIcon className="size-3" />
      Saved{maskedTail ? ` (ends in ${maskedTail})` : ""} · Enter a new value to replace it, or clear the field to remove it
    </p>
  ) : (
    def.help && <p className="text-muted-foreground text-xs">{def.help}</p>
  );

  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{def.label}</Label>
      {control()}
      {hint}
    </div>
  );
}

// FilterSummary collapses a filter into one line so a card shows what the channel sends without expanding.
export function FilterSummary({ filter }: { filter: NotificationFilter }) {
  const parts: string[] = [];
  if (filter.min_severity) {
    parts.push(SEVERITY_OPTIONS.find((o) => o.value === filter.min_severity)?.label ?? filter.min_severity);
  }
  if (filter.vulnclass_include?.length) parts.push(`includes ${filter.vulnclass_include.length} class terms`);
  if (filter.vulnclass_exclude?.length) parts.push(`excludes ${filter.vulnclass_exclude.length} terms`);
  if (filter.task_ids?.length) parts.push(`${filter.task_ids.length} tasks`);
  if (filter.asset_ids?.length) parts.push(`${filter.asset_ids.length} assets`);
  if (filter.on_status_change) parts.push("includes status changes");
  if (parts.length === 0) {
    return <p className="text-muted-foreground text-sm">All findings</p>;
  }
  return <p className="text-muted-foreground text-sm">{parts.join(" · ")}</p>;
}
