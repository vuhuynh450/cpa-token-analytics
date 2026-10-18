import { Languages } from "lucide-react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useI18n } from "@/i18n/react.jsx";

export function LanguageSwitcher() {
  const { locale, setLocale, t } = useI18n();
  return (
    <Select value={locale} onValueChange={setLocale}>
      <SelectTrigger
        size="sm"
        className="w-36 bg-transparent"
        aria-label={t("lang.aria")}
      >
        <Languages className="size-3.5" />
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="en">{t("lang.en")}</SelectItem>
        <SelectItem value="vi">{t("lang.vi")}</SelectItem>
      </SelectContent>
    </Select>
  );
}
