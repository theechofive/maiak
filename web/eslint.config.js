import js from "@eslint/js";
import jsxA11y from "eslint-plugin-jsx-a11y";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist", "node_modules"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  reactHooks.configs.flat.recommended,
  jsxA11y.flatConfigs.recommended,
  {
    files: ["**/*.{ts,tsx}", "public/**/*.js"],
    languageOptions: { globals: globals.browser },
  },
  {
    // The design system (docs/design-system.md): pages compose kit components and use tokens.
    files: ["src/**/*.tsx"],
    ignores: ["src/components/ui/**", "**/*.test.tsx"],
    rules: {
      "no-restricted-syntax": [
        "error",
        {
          selector: "JSXOpeningElement[name.name=/^(button|input|select|textarea|table|dialog)$/]",
          message:
            "Use the component from src/components/ui (Button, Input, Select, Textarea, Table, ConfirmDialog). If the kit lacks one, add it there.",
        },
        {
          selector:
            "Literal[value=/(^|[\\s:])(bg|text|border|fill|stroke|outline|ring|shadow|decoration|accent|caret|from|via|to)-\\[(#|rgba?\\(|hsla?\\(|oklch\\(|oklab\\()/]",
          message: "Use a design token (bg-muted, text-down, ...), not an arbitrary colour.",
        },
        {
          selector:
            "JSXAttribute[name.name='style'] Literal[value=/(#[0-9a-fA-F]{3,8}\\b|rgba?\\(|hsla?\\(|oklch\\(|oklab\\()/]",
          message: "Use a design token (var(--muted), ...), not a colour value in style.",
        },
        {
          selector:
            "TemplateElement[value.raw=/(^|[\\s:])(bg|text|border|fill|stroke|outline|ring|shadow|decoration|accent|caret|from|via|to)-\\[(#|rgba?\\(|hsla?\\(|oklch\\(|oklab\\()/]",
          message: "Use a design token (bg-muted, text-down, ...), not an arbitrary colour.",
        },
      ],
    },
  },
  {
    files: ["vite.config.ts", "eslint.config.js"],
    languageOptions: { globals: globals.node },
  },
);
