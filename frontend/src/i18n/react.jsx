import React, {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import {
  createFormatters,
  dateFnsLocale,
  detectLocale,
  intlLocale,
  STORAGE_KEY,
  translate,
} from "./index.js";

const I18nContext = createContext(null);

export function I18nProvider({ children }) {
  const [locale, setLocaleState] = useState(() =>
    detectLocale(
      localStorage.getItem(STORAGE_KEY),
      navigator.language,
    ),
  );
  const setLocale = (next) => {
    setLocaleState(next);
    localStorage.setItem(STORAGE_KEY, next);
  };
  useEffect(() => {
    document.documentElement.lang = locale;
    document.title = translate(locale, "app.title");
  }, [locale]);
  const value = useMemo(() => {
    const { fmt, compact } = createFormatters(locale);
    return {
      locale,
      setLocale,
      t: (key, params) => translate(locale, key, params),
      intlLocale: intlLocale(locale),
      dateFnsLocale: dateFnsLocale(locale),
      fmt,
      compact,
    };
  }, [locale]);
  return (
    <I18nContext.Provider value={value}>{children}</I18nContext.Provider>
  );
}

export function useI18n() {
  const ctx = useContext(I18nContext);
  if (!ctx) throw new Error("useI18n must be used within I18nProvider");
  return ctx;
}
