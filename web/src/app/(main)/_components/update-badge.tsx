"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * "New version available" indicator in the top bar: checked once on each full
 * page load, and lit up next to the version number when an update is available;
 * clicking it goes straight to the "Version & Updates" card on the system
 * settings page.
 *
 * The backend caches GitHub query results for 30 minutes, so checking once per
 * mount here is safe — the unauthenticated GitHub API only allows 60 requests
 * per hour per IP, and without that cache layer opening a few extra tabs would
 * burn through the quota, making a genuine update check fail right when it's
 * needed.
 *
 * Failures are always silent: the top bar isn't the place for error messages;
 * the user can click "Check for updates" on the settings page to see why.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already accounts for "version numbers are comparable", so dev
        // builds won't light up this indicator.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Silent: neither being offline nor GitHub rate-limiting should pop an error in the top bar.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`New version ${latest} available, click to update`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* Pulsing dot: the top bar is crowded, so plain text is easy to miss; the animation makes it noticeable at a glance. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">New version {latest}</span>
      <span className="sm:hidden">Update</span>
    </Link>
  );
}
