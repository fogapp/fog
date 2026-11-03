// Lint exists for the two rules tsc can't check: hooks called
// unconditionally, and effect/callback dependency lists that match what the
// body reads. Everything else is tsc's job, so nothing else is turned on —
// except one fence around the Settings pages, which were rebuilt on the
// shared controls and should stay that way: a hand-rolled button, select
// or checkbox there, or the browser's confirm(), is what made every page
// look like a different product.
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";

export default tseslint.config(
  { ignores: ["dist/**", ".ds-types/**", "node_modules/**"] },
  {
    files: ["src/**/*.{ts,tsx}", "setup/**/*.{ts,tsx}"],
    languageOptions: { parser: tseslint.parser },
    plugins: { "react-hooks": reactHooks },
    linterOptions: { reportUnusedDisableDirectives: "error" },
    rules: {
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/exhaustive-deps": "error",
    },
  },
  {
    files: ["src/pages/settings/**/*.tsx"],
    rules: {
      "no-restricted-syntax": [
        "error",
        { selector: "JSXOpeningElement[name.name='button']", message: "Use Button (or Segmented) from components — Settings pages share one set of controls." },
        { selector: "JSXOpeningElement[name.name='select']", message: "Use Select from components/Field." },
        { selector: "JSXOpeningElement[name.name='textarea']", message: "Use TextArea from components/Field." },
        { selector: "JSXOpeningElement[name.name='input']", message: "Use Field, FilterInput, or Toggle (in a SettingRow when it is a preference)." },
        { selector: "CallExpression[callee.name='confirm']", message: "Use useConfirm from components/ConfirmDialog." },
      ],
    },
  },
);
