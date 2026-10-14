"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * Header "new version" hint: checked once when the page loads. When an update
 * exists it appears next to the version, and clicking it opens the Version &
 * updates card on the system settings page.
 *
 * The backend caches the GitHub lookup for 30 minutes, so checking on every
 * mount is safe. The unauthenticated GitHub API allows 60 requests per hour
 * per IP; without that cache, a few extra tabs would burn the quota and a
 * real update check would fail.
 *
 * Lookup failures stay silent. The header is not the place for errors; the
 * user sees the reason when they open Settings and click Check for updates.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already means the versions are comparable, so dev builds stay dark.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Stay silent: offline or GitHub rate limits should not toast in the header.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`Version ${latest} is available. Click to update.`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* Pulse dot: the header is crowded, so motion makes the hint visible. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">Update {latest}</span>
      <span className="sm:hidden">Update</span>
    </Link>
  );
}
