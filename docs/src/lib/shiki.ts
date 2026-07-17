import { createShikiFactory } from "fumadocs-core/highlight/shiki";

// A shiki highlighter limited to the languages the docs (and AI answers)
// actually use. The default factory (fumadocs-core/highlight/shiki/full)
// imports "shiki", which bundles every one of shiki's ~250 grammars into the
// runtime worker; that pushed the Cloudflare worker past its 3 MiB size limit.
// Building from shiki/core with explicit grammar imports bundles only these
// languages. Anything not listed falls back to plaintext (fumadocs'
// fallbackLanguage), so this only affects highlight fidelity, never output.
//
// If a new fenced-code language shows up in the docs, add its grammar here.
export const docsShikiFactory = createShikiFactory({
  async init(options) {
    const [{ createHighlighterCore }, { createJavaScriptRegexEngine }] =
      await Promise.all([
        import("shiki/core"),
        import("shiki/engine/javascript"),
      ]);

    const langs = await Promise.all([
      import("@shikijs/langs/bash"),
      import("@shikijs/langs/go"),
      import("@shikijs/langs/typescript"),
      import("@shikijs/langs/tsx"),
      import("@shikijs/langs/javascript"),
      import("@shikijs/langs/jsx"),
      import("@shikijs/langs/json"),
      import("@shikijs/langs/jsonc"),
      import("@shikijs/langs/python"),
      import("@shikijs/langs/yaml"),
      import("@shikijs/langs/toml"),
      import("@shikijs/langs/markdown"),
      import("@shikijs/langs/diff"),
      import("@shikijs/langs/docker"),
      import("@shikijs/langs/rust"),
      import("@shikijs/langs/sql"),
    ]);

    const themes = await Promise.all([
      import("@shikijs/themes/github-light"),
      import("@shikijs/themes/github-dark"),
    ]);

    return createHighlighterCore({
      langs: langs.map((m) => m.default),
      themes: themes.map((m) => m.default),
      langAlias: options?.langAlias,
      engine: createJavaScriptRegexEngine(),
    });
  },
});
