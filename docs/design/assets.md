# Brand assets

All files live in `assets/`. Geometry comes from the canonical three-track mark in `brief.md` section 1. Text is converted to outlines (Helvetica Neue Bold, no embedded fonts), so every file renders identically everywhere. Demo labels only (`yard-demo`, `agent/parser`, PR #42); no real data.

| File | Use |
| --- | --- |
| `logo-mark.svg` | Bare mark, `currentColor`, 16x16 viewBox. Inline anywhere. |
| `favicon.svg` | Browser tab icon; cobalt on light, pale blue on dark via an internal `prefers-color-scheme` rule. |
| `favicon-32.png` | Raster fallback, mark on chalk tile. |
| `logo-light.svg` / `logo-dark.svg` | Mark and "agentyard" wordmark lockup for README `<picture>`. |
| `icon.png` / `icon-dark.png` | 1024px macOS-style squircle app icon, light and dark. |
| `banner-light.png` / `banner-dark.png` | 1600x560 README hero: lockup, headline, composed demo panel. |
| `social-preview.png` | 1280x640 GitHub social card (upload in repo Settings). |

README snippet:

```html
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/banner-dark.png">
  <img alt="agentyard: worktrees, pull requests, and agent sessions in one calm dashboard." src="assets/banner-light.png">
</picture>
```

Regenerating: the files are produced by a deterministic script (fontTools outlines, rsvg-convert), following the brief's rule that generated art is draft-only and exact paths and type are composited afterward. Codex image generation was not used, to keep the mark exact.
