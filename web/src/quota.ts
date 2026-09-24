// Adapted from the Copilot quota adapter in CPA Management Center (MIT).
export type Bucket = {
  window: string;
  description?: string;
  remainingFraction: number;
  resetTime?: string;
};
export type Quota = {
  subscription?: { plan?: string; tierName?: string };
  groups?: { buckets?: Bucket[] }[];
  summary?: { key: string; value: number }[];
  stale?: boolean;
  observedAt?: string;
};
export function usedPercent(remaining: number): number {
  return Math.floor(Math.max(0, Math.min(100, 100 - remaining * 100)));
}
export function resetLabel(
  value?: string,
  locale = "en-US",
  timeZone?: string,
): string {
  if (!value) return "";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime()) || date.getUTCFullYear() < 2000)
    return "";
  const dateOnly = /^\d{4}-\d{2}-\d{2}$/.test(value);
  const day = new Intl.DateTimeFormat(locale, {
    month: "short",
    day: "numeric",
    timeZone: dateOnly ? "UTC" : timeZone,
  }).format(date);
  return dateOnly
    ? `Resets ${day}`
    : `Resets ${day} at ${new Intl.DateTimeFormat(locale, { hour: "numeric", minute: "2-digit", timeZone }).format(date)}`;
}
export function buckets(quota: Quota): Bucket[] {
  return (quota.groups ?? [])
    .flatMap((g) => g.buckets ?? [])
    .filter(
      (b) =>
        typeof b.remainingFraction === "number" &&
        Number.isFinite(b.remainingFraction),
    );
}
