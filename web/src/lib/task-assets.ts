import type { NewAssetType } from "@/lib/types";

const ASSET_TYPE_LABELS: Record<NewAssetType, string> = {
  app: "App",
  endpoint: "Endpoint",
  ip: "IP",
  root_domain: "Root domain",
  service: "Service",
  subdomain: "Subdomain",
};

const TASK_ASSET_SOURCE_LABELS: Record<string, string> = {
  agent: "Discovered by agent",
  anchor: "Blackboard anchor",
  api: "Asset API",
  company: "Company link",
  legacy: "Legacy link",
  manual: "Added manually",
  system: "System link",
  task: "Task setup",
};

export function taskAssetTypeLabel(type: NewAssetType): string {
  return ASSET_TYPE_LABELS[type];
}

export function taskAssetSourceLabel(source: string): string {
  return TASK_ASSET_SOURCE_LABELS[source] ?? source;
}
