import { test, expect } from "bun:test";
import { usedPercent, resetLabel, buckets } from "./quota";
test("credits and budget percentages", () => {
  expect(usedPercent(0.96)).toBe(4);
  expect(usedPercent(1)).toBe(0);
  expect(usedPercent(0.0099)).toBe(99);
});
test("reset timezone and date-only precision", () => {
  expect(resetLabel("2026-10-01T00:00:00Z", "en-US", "Asia/Shanghai")).toBe(
    "Resets Oct 1 at 8:00 AM",
  );
  expect(resetLabel("2026-10-01", "en-US", "America/Los_Angeles")).toBe(
    "Resets Oct 1",
  );
  expect(resetLabel("invalid")).toBe("");
});
test("unknown quota is not zero", () => {
  expect(buckets({})).toEqual([]);
  expect(
    buckets({
      groups: [{ buckets: [{ window: "credits", remainingFraction: NaN }] }],
    }),
  ).toEqual([]);
});
