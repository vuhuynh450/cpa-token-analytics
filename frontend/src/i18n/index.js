import { enUS, vi } from "date-fns/locale";
import { createFormatters } from "../analytics.js";
import en from "./en.js";
import viDict from "./vi.js";

export { createFormatters };

export const LOCALES = ["en", "vi"];
export const STORAGE_KEY = "cpa-usage-locale";

const DICTS = { en, vi: viDict };

export function detectLocale(saved, navLang) {
  if (LOCALES.includes(saved)) return saved;
  if (typeof navLang === "string" && navLang.toLowerCase().startsWith("vi")) {
    return "vi";
  }
  return "en";
}

function lookup(dict, key) {
  let node = dict;
  for (const part of key.split(".")) {
    if (node == null || typeof node !== "object") return undefined;
    node = node[part];
  }
  return typeof node === "string" ? node : undefined;
}

export function translate(locale, key, params) {
  let value = lookup(DICTS[locale], key);
  if (value === undefined) value = lookup(DICTS.en, key);
  if (value === undefined) return key;
  if (!params) return value;
  return value.replace(/\{(\w+)\}/g, (m, name) =>
    params[name] !== undefined ? String(params[name]) : m,
  );
}

// Strings the Go backend embeds in API responses, keyed by exact value.
// Kept as \u escapes so the frontend source stays free of CJK literals
// (see the no-Han grep check); originals are in plugin/main.go.
const BACKEND_STRINGS = {
  "\u672a\u77e5 Key": { en: "Unknown Key", vi: "Khóa không xác định" },
  "\u65e0\u6548\u65f6\u95f4\u7c92\u5ea6": {
    en: "Invalid time granularity",
    vi: "Độ chi tiết thời gian không hợp lệ",
  },
  "\u65e0\u6548\u5f00\u59cb\u65e5\u671f": { en: "Invalid start date", vi: "Ngày bắt đầu không hợp lệ" },
  "\u65e0\u6548\u7ed3\u675f\u65e5\u671f": { en: "Invalid end date", vi: "Ngày kết thúc không hợp lệ" },
  "\u5f00\u59cb\u65e5\u671f\u4e0d\u80fd\u665a\u4e8e\u7ed3\u675f\u65e5\u671f": {
    en: "Start date cannot be after end date",
    vi: "Ngày bắt đầu không được sau ngày kết thúc",
  },
};

export function translateBackend(text, locale) {
  const mapped = BACKEND_STRINGS[text];
  if (!mapped) return text;
  return mapped[locale] || mapped.en;
}

export function intlLocale(locale) {
  return locale === "vi" ? "vi-VN" : "en-US";
}

export function dateFnsLocale(locale) {
  return locale === "vi" ? vi : enUS;
}
