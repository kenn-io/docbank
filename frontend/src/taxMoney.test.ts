import { expect, it } from "vite-plus/test";
import { formatTaxAmount, taxStateLabel } from "./taxMoney.js";

const hint = "Run docbank tax record show r1 to see the exact amount";
const cases: [number | null, string, string, ReturnType<typeof formatTaxAmount>][] = [
  [123400, "USD", "payable", { text: "123400 USD", direction: "Payable" }],
  [5000, "JPY", "payable", { text: "5000 JPY", direction: "Payable" }],
  [1234, "QQQ", "refundable", { text: "1234 QQQ", direction: "Refundable" }],
  [9007199254740991, "USD", "payable", { text: "9007199254740991 USD", direction: "Payable" }],
  [9007199254740992, "USD", "payable", { text: "Too large to show", direction: "Payable", hint }],
  [0, "USD", "nil", { text: "0 USD", direction: "Nil" }],
  [null, "USD", "unknown", { text: "Unknown", direction: "Unknown" }],
  [100, "USD", "future_direction", { text: "100 USD", direction: "Future direction" }],
];

it.each(cases)("formats %s %s %s exactly", (minor, currency, direction, expected) => {
  expect(formatTaxAmount(minor, currency, direction, "r1")).toStrictEqual(expected);
});

it.each([
  ["expected", "Expected"],
  ["information_requested", "Information requested"],
  ["not_required", "Not required"],
])("labels %s as %s", (state, label) => {
  expect(taxStateLabel(state)).toBe(label);
});
