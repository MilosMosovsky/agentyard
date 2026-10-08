# Brand assets

Brand files live in `assets/`; app screenshots live in `docs/screenshots/`. Geometry comes from the canonical three-track mark in `assets/logo-mark.svg`. Text is converted to outlines (Helvetica Neue Bold, no embedded fonts), so every file renders identically everywhere. Demo labels only (`yard-demo`, `agent/parser`, PR #42); no real data.

| File | Use |
| --- | --- |
| `logo-mark.svg` | Bare mark, `currentColor`, 16x16 viewBox. Inline anywhere. |
| `favicon.svg` | Browser tab icon; cobalt on light, pale blue on dark via an internal `prefers-color-scheme` rule. |
| `favicon-32.png` | Raster fallback, mark on chalk tile. |
| `logo-light.svg` / `logo-dark.svg` | Mark and "agentyard" wordmark lockup for README `<picture>`. |
| `icon.png` / `icon-dark.png` | 1024px macOS-style squircle app icon, light and dark. |
| `banner-light.png` / `banner-dark.png` | 1600x560 alternate brand banners: lockup, headline, composed demo panel. |
| `social-preview.png` | 1280x640 GitHub social card (upload in repo Settings). |

The README leads with the Pull requests appshot, not a brand banner or storage view. The
dark/light pair lives in `docs/screenshots/prs-appshot-{dark,light}.jpg`. The plain PR captures
remain in the feature tour; Worktrees and Sessions screenshots stay with their sections.

README hero:

```html
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/prs-appshot-dark.jpg">
  <img alt="agentyard Pull requests in a browser window." src="docs/screenshots/prs-appshot-light.jpg">
</picture>
```

Regenerating: the files are produced by a deterministic script (fontTools outlines, rsvg-convert), following the brief's rule that generated art is draft-only and exact paths and type are composited afterward. Codex image generation was not used, to keep the mark exact.

## README appshot

`docs/screenshots/appshot.html` owns the browser frame. It uses the existing synthetic
`prs-dark.jpg` / `prs-light.jpg` captures without changing the application inside them.
Serve the repository with a local static server, open
`/docs/screenshots/appshot.html?theme=dark` (or `light`), and save a full-page browser screenshot
to the matching `prs-appshot-*.jpg`. The composition is 1536 pixels wide, including a 48-pixel
outer margin. The frame is presentation chrome; its address is a label, not a link.

## Session agent marks

The inline `page.html` SVG sprite includes the Claude and OpenAI marks from
[Simple Icons 13.21.0](https://github.com/simple-icons/simple-icons/tree/13.21.0/icons),
licensed under [CC0-1.0](https://github.com/simple-icons/simple-icons/blob/13.21.0/LICENSE.md).
Claude uses `claude.svg`; Codex uses `openai.svg`. The shared `tool-icon` template owns
all instances. Brand marks remain the property of their respective owners.
