// Shared logic for the "send an asset straight into a New Task" flow.
//
// The Assets list builds a deep-link from an asset (its target string + the
// exact allow-rule kind); the Tasks page reads that link back, substitutes the
// target into the chosen template, and constructs the exact allow-rule. Both
// sides import from here so they can never disagree on what an asset maps to.
//
// Design: docs/plans/2026-10-15-asset-to-task-design.md

import type { Asset, AssetInterceptRuleInput } from "@/lib/types";

// The allow-rule kinds this flow produces. Always an EXACT match: a single
// asset, never a fuzzy subtree, so a deep-dive task can only touch what was
// clicked. (A fuzzy/subtree variant is deliberately out of scope for v1.)
export type AssetSeedKind = "exact_url" | "exact_domain" | "exact_ip";

export interface AssetSeed {
  target: string;
  kind: AssetSeedKind;
}

// Query-param names carried on the /function/tasks deep-link.
export const SEED_PARAM = {
  flag: "new", // "1" => open the New Task sheet
  template: "seedTpl", // task template id to pre-apply
  assetId: "seedAssetId", // asset id to attach as the task's sole target
  target: "seedTarget", // the substituted {{TARGET}} value (url/domain/ip)
  kind: "seedKind", // the exact allow-rule kind for `target`
  draft: "seedDraft", // task-draft id to fire (suggest_task); seeds the whole sheet
} as const;

// The placeholder a template uses to receive the clicked asset's identity.
export const TARGET_TOKEN = "{{TARGET}}";

// Map an asset to its most specific identity + the exact allow-rule kind.
// Returns null when the asset has no usable identity for its type, in which
// case the Assets UI disables the "Send to task" action (never seeds an empty
// target or an empty allow-rule). Only the four target-bearing asset types are
// supported in v1 (app/service are intentionally excluded).
export function assetSeed(asset: Asset): AssetSeed | null {
  switch (asset.type) {
    case "endpoint": {
      const target = (asset.url ?? "").trim();
      return target ? { target, kind: "exact_url" } : null;
    }
    case "subdomain": {
      const target = (asset.domain ?? "").trim();
      return target ? { target, kind: "exact_domain" } : null;
    }
    case "root_domain": {
      const target = (asset.domain ?? asset.root_domain ?? "").trim();
      return target ? { target, kind: "exact_domain" } : null;
    }
    case "ip": {
      const target = (asset.ip ?? "").trim();
      return target ? { target, kind: "exact_ip" } : null;
    }
    default:
      return null;
  }
}

// Validate a kind arriving from the URL (untrusted) before it becomes a rule.
export function isAssetSeedKind(value: string): value is AssetSeedKind {
  return value === "exact_url" || value === "exact_domain" || value === "exact_ip";
}

// Replace every {{TARGET}} placeholder in template text with the asset target.
export function substituteTarget(text: string, target: string): string {
  return text.split(TARGET_TOKEN).join(target);
}

// Build the single exact allow-rule that locks a task to one asset. Any allow
// rule flips the task's whitelist ON, which hard-excludes every other asset
// (e.g. another company's assets in the shared store).
export function buildAllowRule(target: string, kind: AssetSeedKind): AssetInterceptRuleInput {
  return { action: "allow", kind, pattern: target, note: "", enabled: true };
}

// Build the /function/tasks deep-link query string for an asset + template.
export function buildSeedQuery(templateId: number, assetId: number, seed: AssetSeed): string {
  const q = new URLSearchParams();
  q.set(SEED_PARAM.flag, "1");
  q.set(SEED_PARAM.template, String(templateId));
  q.set(SEED_PARAM.assetId, String(assetId));
  q.set(SEED_PARAM.target, seed.target);
  q.set(SEED_PARAM.kind, seed.kind);
  return q.toString();
}
