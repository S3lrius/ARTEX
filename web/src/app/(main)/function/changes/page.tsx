"use client";

import * as React from "react";

import { useRouter } from "next/navigation";

import { CheckIcon, GitCompareIcon, RefreshCwIcon, TargetIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api } from "@/lib/api";
import { SEED_PARAM, type AssetSeedKind } from "@/lib/asset-task-seed";
import type { AssetChange, Company } from "@/lib/types";
import { cn } from "@/lib/utils";

const CHANGE_TYPES = ["new", "changed", "reappeared", "gone"] as const;

// Visual style per change type (matches the design doc's taxonomy colors).
const CHANGE_STYLE: Record<AssetChange["change_type"], string> = {
  new: "border-green-600/40 bg-green-500/10 text-green-600 dark:text-green-400",
  changed: "border-amber-600/40 bg-amber-500/10 text-amber-600 dark:text-amber-400",
  reappeared: "border-blue-600/40 bg-blue-500/10 text-blue-600 dark:text-blue-400",
  gone: "border-muted-foreground/30 bg-muted text-muted-foreground",
};

// Map an asset type to the exact allow-rule kind used by the New Task seed flow.
// Returns null when "Fire deep-dive" can't build a clean single-asset scope.
function seedKindFor(assetType: string, label: string): AssetSeedKind | null {
  switch (assetType) {
    case "endpoint":
      return "exact_url";
    case "subdomain":
    case "root_domain":
      return "exact_domain";
    case "ip":
      return "exact_ip";
    case "service":
      return label.startsWith("http") ? "exact_url" : null;
    default:
      return null;
  }
}

function describeChange(c: AssetChange): string {
  switch (c.change_type) {
    case "new":
      return `new ${c.asset_type}`;
    case "changed":
      return `${c.field}: ${c.old_value || "∅"} → ${c.new_value || "∅"}`;
    case "reappeared":
      return "back online after going quiet";
    case "gone":
      return "not seen recently";
  }
}

export default function ChangesPage() {
  const router = useRouter();
  const [changes, setChanges] = React.useState<AssetChange[]>([]);
  const [companies, setCompanies] = React.useState<Company[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [companyFilter, setCompanyFilter] = React.useState<string>("all");
  const [typeFilter, setTypeFilter] = React.useState<string>("all");
  const [unseenOnly, setUnseenOnly] = React.useState(false);

  const companyName = React.useMemo(() => {
    const m = new Map<number, string>();
    for (const c of companies) m.set(c.id, c.name);
    return m;
  }, [companies]);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const rows = await api.assetChanges({
        companyId: companyFilter === "all" ? undefined : Number(companyFilter),
        type: typeFilter === "all" ? undefined : typeFilter,
        unseen: unseenOnly || undefined,
      });
      setChanges(rows);
    } catch (e) {
      toast.error(`Couldn't load changes: ${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, [companyFilter, typeFilter, unseenOnly]);

  React.useEffect(() => {
    void load();
  }, [load]);

  React.useEffect(() => {
    api
      .companies()
      .then(setCompanies)
      .catch(() => setCompanies([]));
  }, []);

  const markSeen = async (id: number) => {
    try {
      await api.markAssetChangeSeen(id);
      setChanges((prev) => prev.map((c) => (c.id === id ? { ...c, seen: true } : c)));
    } catch (e) {
      toast.error(`Couldn't mark seen: ${(e as Error).message}`);
    }
  };

  const markAllSeen = async () => {
    try {
      const { cleared } = await api.markAllAssetChangesSeen(
        companyFilter === "all" ? undefined : Number(companyFilter),
      );
      toast.success(`Marked ${cleared} change${cleared === 1 ? "" : "s"} seen`);
      void load();
    } catch (e) {
      toast.error(`Couldn't clear: ${(e as Error).message}`);
    }
  };

  // Seed a pre-filled New Task scoped to just this asset (reuses the asset→task
  // deep-link; the New Task sheet lets the user pick a template there).
  const fireDeepDive = (c: AssetChange) => {
    const kind = seedKindFor(c.asset_type, c.label);
    if (!kind || !c.asset_id) {
      toast.error("Can't scope a deep dive from this asset type");
      return;
    }
    const q = new URLSearchParams();
    q.set(SEED_PARAM.flag, "1");
    q.set(SEED_PARAM.assetId, String(c.asset_id));
    q.set(SEED_PARAM.target, c.label);
    q.set(SEED_PARAM.kind, kind);
    router.push(`/function/tasks?${q.toString()}`);
  };

  const unseenCount = changes.filter((c) => !c.seen).length;

  return (
    <div className="flex flex-1 flex-col gap-4 p-4 md:p-6">
      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <CardTitle className="flex items-center gap-2">
                <GitCompareIcon className="size-5" /> Changes
              </CardTitle>
              <CardDescription>
                What&apos;s different in the asset store — new, changed, reappeared, or gone assets.
              </CardDescription>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Select value={companyFilter} onValueChange={setCompanyFilter}>
                <SelectTrigger className="h-8 w-44">
                  <SelectValue placeholder="All companies" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="all">All companies</SelectItem>
                  {companies.map((c) => (
                    <SelectItem key={c.id} value={String(c.id)}>
                      {c.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Select value={typeFilter} onValueChange={setTypeFilter}>
                <SelectTrigger className="h-8 w-36">
                  <SelectValue placeholder="All changes" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="all">All changes</SelectItem>
                  {CHANGE_TYPES.map((t) => (
                    <SelectItem key={t} value={t}>
                      {t}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Button
                variant={unseenOnly ? "default" : "outline"}
                size="sm"
                onClick={() => setUnseenOnly((v) => !v)}
              >
                Unseen only
              </Button>
              <Button variant="outline" size="sm" onClick={() => void load()}>
                <RefreshCwIcon className="size-4" /> Refresh
              </Button>
              <Button variant="outline" size="sm" onClick={() => void markAllSeen()} disabled={unseenCount === 0}>
                <CheckIcon className="size-4" /> Mark all seen
              </Button>
            </div>
          </div>
        </CardHeader>
        <CardContent>
          {loading ? (
            <div className="flex items-center justify-center py-12 text-muted-foreground">
              <Spinner className="mr-2 size-4" /> Loading changes…
            </div>
          ) : changes.length === 0 ? (
            <div className="py-12 text-center text-sm text-muted-foreground">
              No changes detected. Re-run recon on your in-scope companies and anything different will show up here.
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-44">Detected</TableHead>
                  <TableHead className="w-36">Company</TableHead>
                  <TableHead>Asset</TableHead>
                  <TableHead>Change</TableHead>
                  <TableHead className="w-44 text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {changes.map((c) => {
                  const canFire = Boolean(seedKindFor(c.asset_type, c.label) && c.asset_id);
                  return (
                    <TableRow key={c.id} className={cn(!c.seen && "bg-primary/5")}>
                      <TableCell className="text-xs text-muted-foreground">
                        {new Date(c.detected_at).toLocaleString()}
                      </TableCell>
                      <TableCell className="text-sm">
                        {c.company_id ? (companyName.get(c.company_id) ?? `#${c.company_id}`) : "—"}
                      </TableCell>
                      <TableCell className="max-w-[320px]">
                        <div className="truncate font-mono text-xs" title={c.label}>
                          {c.label}
                        </div>
                        <div className="text-[10px] text-muted-foreground">{c.asset_type}</div>
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center gap-2">
                          <Badge variant="outline" className={cn("capitalize", CHANGE_STYLE[c.change_type])}>
                            {c.change_type}
                          </Badge>
                          <span className="font-mono text-xs text-muted-foreground">{describeChange(c)}</span>
                        </div>
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center justify-end gap-1">
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={() => fireDeepDive(c)}
                            disabled={!canFire}
                            title={canFire ? "Fire a scoped deep dive on this asset" : "Can't scope this asset type"}
                          >
                            <TargetIcon className="size-4" /> Fire deep-dive
                          </Button>
                          {!c.seen && (
                            <Button variant="ghost" size="icon" onClick={() => void markSeen(c.id)} title="Mark seen">
                              <CheckIcon className="size-4" />
                            </Button>
                          )}
                        </div>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
