"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** How long to wait for the new version to come up. An upgrade starts the process
 *  three times (stage, swap, new version), each in seconds. Three minutes covers
 *  a slow disk and a Docker container rebuild. */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(false);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // Separate from progress: once staging finishes the process is gone and SSE drops, so switch to polling /api/health.
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // Opening this page does not contact GitHub. Check for updates is the only
  // lookup, and it skips the cache so the result is current.
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("Couldn't check for updates: " + r.error);
          else if (r.has_update) toast.success(`Version ${r.latest} is available`);
          else if (r.comparable) toast.success("You're on the latest version");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("Couldn't check for updates: " + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  // Poll /api/health until the version changes.
  //
  // The signal has to be "the version changed", not "it answered". During the
  // swap the old version comes back briefly (only to replace artex.new, then it
  // exits). Connectivity alone would look like success.
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`Updated to ${j.version}. Reloading…`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // Connection failures during the restart window are expected. Keep polling.
      }
    }
    setRestarting(false);
    toast.error("Timed out waiting for the service to restart. Check the backend logs, and confirm ARTEX was started with start.sh or start.bat.");
  }, []);

  // Subscribe to update progress. SSE does not go through the Next /api rewrite, which buffers and never flushes events.
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("Update failed: " + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // SSE always drops when the process exits. If we are already waiting for
        // the restart, that is expected; /api/health polling decides the outcome.
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `Update to ${info.latest}?\n\n` +
        "The app will restart and running tasks will be interrupted.\n" +
        (info.mode === "docker"
          ? "\nIn a container this only replaces the program, not Playwright, nmap, or the rest of the image toolchain. " +
            "If the new version needs new tools, rebuild with docker compose build instead."
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "Preparing…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error("Couldn't start the update: " + (e as Error).message);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "Roll back to the previous version?\n\nThe app will restart and running tasks will be interrupted.\nThe database schema is not rolled back, so the older version may not understand data written by the new one.",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("Switched to the previous version. Restarting…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("Couldn't roll back: " + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // Only the download has a real percent (from Content-Length). Verify, extract,
  // and waiting for restart have unknown duration, so the bar fills and pulses
  // to show work is in progress without a time estimate.
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // Settings is a multi-column masonry layout. The card owns its row spacing and must not break across columns (see the comment in page.tsx).
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          Version and updates
        </CardTitle>
        <CardDescription>
          This app does not check for or download updates on its own. Click Check for updates to look at this fork&apos;s GitHub releases. Installing a version restarts the app and interrupts running tasks.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">Current</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "Standalone"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">Latest</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              Release notes <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            Couldn't reach GitHub: {info.error}
            {" "}Configure the global proxy above and try again.
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.latest} has no {info.os}/{info.arch} package (missing {info.asset}), so it cannot update automatically.
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            Will download <span className="font-mono">{info.asset}</span>
            {info.size ? ` (${humanSize(info.size)})` : ""}. It is swapped in only after the SHA256 check and a smoke test. A failure keeps the current version.
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            You're on the latest version.
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            An update inside Docker only replaces the program, not Playwright, nmap, or the rest of the image toolchain. Running
            <span className="font-mono"> docker compose up -d </span>
            recreates the container from the image you last built. To pick up source changes, rebuild it:
            <span className="font-mono"> docker compose build artex &amp;&amp; docker compose up -d artex</span>.
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting ? "Restarting to apply the new version. The page will refresh…" : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            Check for updates
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `Update to ${info.latest}` : "Update now"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              Roll back to the previous version
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          One-click update relies on the supervisor script to restart the app. Start ARTEX with <span className="font-mono">start.sh</span> (or
          <span className="font-mono"> start.bat</span> on Windows). If you run the artex binary directly, it will not be started again after it exits.
        </p>
      </CardContent>
    </Card>
  );
}
