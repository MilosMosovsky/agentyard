# agentyard — design brief

This is the visual and interaction specification for agentyard. It is based on `README.md` and `page.html`, read as source only. No application was run and no worktrees, PRs, sessions, transcripts, or other real user data were inspected. The source still uses the name worktreesd; use **agentyard** for the identity specified here.

The product remains one Go binary using the standard library, one embedded `html/template` page, inline CSS, and small native JavaScript functions. No JS framework, package install, frontend build, remote fonts, CDN, or runtime asset service. It starts through a macOS LaunchAgent and serves `http://<name>.local`. These constraints are part of the design.

## 1. Identity

### Feel

**A well-kept working yard: many independent jobs, one legible place.** The interface feels quiet, exact, and already familiar to a developer. Give identity and navigation room; spend density on evidence and decisions. A person must answer “What can I clear?”, “What is blocking this PR?”, and “How do I resume this?” without opening a second page.

Use these references for specific qualities: Linear's alignment and compact scanning; Vercel's restraint; Raycast's immediate keyboard access; the Codex app's readable session context; Things' calm grouping and deliberate spacing. Draw the mark and interface independently. No copied silhouettes, proprietary icons, interface replicas, gradients, glass panels, decorative terminal chrome, mascots, sparkles, or animated activity wallpaper.

Light mode is chalk white with cool graphite ink. Dark mode is charcoal with pale ink. Blue belongs to navigation, focus, and the Open PR state. Other colors communicate meaning only. Keep data surfaces flat; reserve elevation for things that actually overlap.

### Mark: three tracks with offset stops

Draw three separate horizontal tracks with small downward terminal stops. Their unequal lengths express parallel work occupying different positions in the same yard. All tracks start on the same x coordinate. The result is asymmetric, orthogonal, and readable without a container.

The canonical mark uses a `16 × 16` viewBox and these **filled shapes**, not stroked lines:

```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16">
  <path fill="currentColor"
    d="M2 2H8V5H6V4H2Z
       M2 7H11V10H9V9H2Z
       M2 12H14V15H12V14H2Z"/>
</svg>
```

- Horizontal lane thickness: `2px` at favicon size. Lane tops: `y=2`, `7`, `12`. Right endpoints: `x=8`, `11`, `14`. Each terminal extends `1px` below its lane and is `2px` wide. Square corners throughout.
- Keep the three shapes disjoint. Do not connect their left edges, rotate them, add circles, draw a branch graph, or enclose them in a letter. The open gaps and offset square stops are the identifying geometry.
- At `16px` and `32px`, preserve the integer coordinates and use filled paths. Do not add detail. At `24px`, scale the same geometry by `1.5`; do not round individual points independently.
- Monochrome mark: `--text-1`. Promotional mark: `--accent`. Never color the three tracks separately. Do not turn the shape into a status indicator.
- Minimum standalone size: `16 × 16px`. Clear space on every side outside its viewBox: `4px` at the `16px` size, scaling proportionally. Browser favicons use the viewBox alone.
- The silhouette must stay distinct from the references: no diagonal striped disc, triangle, diagonal arrow bundle, woven knot, or checkmark. Do not substitute any reference brand's mark in a production asset or prompt.

The wordmark pairs the `24 × 24px` mark with the lowercase name **agentyard**, a `10px` gap, `20px` system sans at weight `650`, `24px` line-height, and `-0.4px` letter-spacing. Align the mark and text by their `24px` boxes. Use one color for the complete lockup. On the live page, render the name as accessible text; the adjacent SVG is `aria-hidden="true"`. The name links to Worktrees. Do not stylize “agent” and “yard” differently.

### Taglines

1. **Your coding agents, one calm yard.** — selected primary tagline.
2. Every worktree, PR, and session. One calm page.
3. Parallel work. A clear place to land.
4. The yard where your coding agents work.

Use the selected line in the README hero and social preview. Follow it with: **“Every worktree, pull request, and agent session on one page.”** Keep the tagline off the dashboard itself; there, the work is the introduction.

### Voice

- Sentence case. Short, factual labels. Use “Sync now”, “Copy resume”, “Safe to delete”, “Folder deleted”, and “Last active”. Spell out “Pull requests” in navigation; use “PR” inside rows.
- Report evidence before advice: “Keep · 3 unpushed commits”, “Checks failing · unit-tests”, “Last synced 12 min ago”. Never report “Running” from a recent file timestamp. Recent session activity is not proof of a live process.
- Preserve the five verdict names exactly. “Safe to delete” requires the server's clean-and-merged verdict; “Prunable” describes a missing folder's registration. Never group both under “Safe”.
- Say “Commands are copied, never run here.” once beside cleanup commands. A copy action is not a deletion action. There is no Delete button and no implied automatic cleanup.
- Use “Ready to merge” only for the server's PR state. Zero reported checks is “No checks reported”, not “Checks passing”.
- Errors state what failed, what is still shown, and the available action. Example: “Couldn't refresh 2 repositories. Showing their last successful results. Sync now.” No apologies, jokes, congratulations, or invented reassurance.
- Sizes are logical sizes. Label the cleanup figure “Safe-to-delete size”; its explanation is “Logical size; space freed can be smaller.” Do not promise the displayed bytes will be reclaimed.
- Timestamps use short relative labels in rows and exact local dates in details. Commands, paths, session IDs, and user-authored text retain their exact content.

## 2. Design tokens

### CSS custom properties

The following values are normative. Every component consumes these variables. Define palette values once per theme, then derive state roles through aliases. Never maintain separate color maps for table rows, chips, KPIs, and the drawer.

```css
:root {
  color-scheme: light;

  --bg-canvas: #F7F7F5;
  --bg-surface: #FFFFFF;
  --bg-inset: #F0F1F3;
  --bg-hover: #ECEFF4;
  --bg-selected: #E9EFFB;

  --text-1: #20242C;
  --text-2: #4F5663;
  --text-3: #646C79;
  --line: #DFE2E7;
  --line-control: #7B8391;

  --accent: #2F55B7;
  --on-accent: #FFFFFF;
  --tone-positive: #246347;
  --tone-neutral: #596271;
  --tone-waiting: #79500F;
  --tone-protect: #9F3442;

  --shadow-popover: 0 4px 16px #20242C14;
  --shadow-drawer: -8px 0 32px #20242C1F;
  --backdrop: #1113183D;
}

:root[data-theme="dark"] {
  color-scheme: dark;

  --bg-canvas: #111318;
  --bg-surface: #191C23;
  --bg-inset: #20242C;
  --bg-hover: #282D37;
  --bg-selected: #242F49;

  --text-1: #F2F3F5;
  --text-2: #D1D5DE;
  --text-3: #B8C0CE;
  --line: #333945;
  --line-control: #778398;

  --accent: #C4D6FF;
  --on-accent: #111318;
  --tone-positive: #B2E0C4;
  --tone-neutral: var(--text-2);
  --tone-waiting: #EAD0A8;
  --tone-protect: #FFC1CB;

  --shadow-popover: 0 4px 16px #00000052;
  --shadow-drawer: -8px 0 32px #00000066;
  --backdrop: #00000066;
}

:root {
  --verdict-safe: var(--tone-positive);
  --verdict-prunable: var(--tone-neutral);
  --verdict-not-merged: var(--tone-waiting);
  --verdict-open-pr: var(--accent);
  --verdict-keep: var(--tone-protect);

  --pr-checks-passing: var(--tone-positive);
  --pr-checks-failing: var(--tone-protect);
  --pr-checks-pending: var(--tone-waiting);
  --pr-review-required: var(--tone-waiting);
  --pr-review-approved: var(--tone-positive);
  --pr-review-changes: var(--tone-protect);
  --pr-draft: var(--tone-neutral);
  --pr-conflicts: var(--tone-protect);
  --pr-ready: var(--tone-positive);
  --pr-merge-queue: var(--accent);
  --pr-unknown: var(--tone-neutral);

  --focus-ring: var(--accent);
  --status-bg: var(--bg-surface);
  --radius-none: 0px;
  --radius-sm: 4px;
  --radius-control: 6px;
  --radius-panel: 10px;
  --radius-pill: 999px;

  --space-0: 0px;
  --space-1: 2px;
  --space-2: 4px;
  --space-3: 8px;
  --space-4: 12px;
  --space-5: 16px;
  --space-6: 20px;
  --space-7: 24px;
  --space-8: 32px;
  --space-9: 40px;
  --space-10: 48px;
  --space-11: 64px;

  --border-width: 1px;
  --focus-width: 2px;
  --focus-offset: 2px;
  --shadow-flat: none;

  --duration-hover: 100ms;
  --duration-feedback: 120ms;
  --duration-enter: 180ms;
  --duration-exit: 140ms;
  --ease-standard: cubic-bezier(0.2, 0, 0, 1);
  --ease-enter: cubic-bezier(0.16, 1, 0.3, 1);
  --ease-exit: cubic-bezier(0.4, 0, 1, 1);
  --copy-feedback-duration: 1600ms;
}

@media (prefers-reduced-motion: reduce) {
  :root {
    --duration-hover: 0ms;
    --duration-feedback: 0ms;
    --duration-enter: 0ms;
    --duration-exit: 0ms;
  }
  *, *::before, *::after {
    animation: none !important;
    transition: none !important;
    scroll-behavior: auto !important;
  }
}
```

Use canvas for the page, surface for tables/cards/drawer, inset for command wells and table headers, hover for interactive rows, and selected for the session whose drawer is open. Selection uses a complete background fill. Status chips use opaque `--status-bg` in every row state; they never inherit the selected fill. There are no tinted status backgrounds.

`--line` is a decorative separator, never the only visible boundary of an input or indication of focus. Search fields and outlined controls use `--line-control`. Focus uses a `2px` outline with `2px` offset; maintain at least `4px` clearance from clipping containers. Active tabs also have a `2px` **bottom** indicator in `--text-1`. No state or brand color appears as a left border, pseudo-element rail, or inset stripe anywhere.

Theme preference is **System / Light / Dark**, default System, stored under `agentyard.theme`. One native `applyTheme` function owns initial resolution, menu changes, and `matchMedia` changes. Run the initial resolution in the document head before paint. It writes `data-theme="light"` or `"dark"`; the palette above is the only color owner. Without JavaScript, the page uses the light palette and remains readable.

### Contrast ledger

Values below are calculated from the exact opaque sRGB hex pairs, with WCAG ratios rounded to two decimals and APCA Lc to one decimal. `+Lc` means dark text on light; `−Lc` means light text on dark. The base backgrounds are light surface `#FFFFFF` and dark surface `#191C23`.

Use WCAG 2.2's **4.5:1** minimum for normal text and **3:1** for essential non-text indicators and boundaries. APCA is an additional perceptual diagnostic here, not a WCAG 2.2 conformance test or a ratio. A contrast reading alone does not certify an entire page. [W3C text contrast](https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html), [W3C non-text contrast](https://www.w3.org/WAI/WCAG22/Understanding/non-text-contrast.html).

APCA readings use **apca-w3 0.1.9, SAPC 0.0.98G-4g**, including its black soft clamp and signed polarity. Its font-size/weight guidance is separate from the number; do not label a small label “APCA passed” simply because its Lc exceeds 60. [Reference algorithm](https://raw.githubusercontent.com/Myndex/apca-w3/master/src/apca-w3.js), [APCA usage guidance](https://git.apcacontrast.com/documentation/APCA_in_a_Nutshell.html).

| Foreground role | Light hex | WCAG | APCA Lc | Dark hex | WCAG | APCA Lc |
| --- | --- | ---: | ---: | --- | ---: | ---: |
| Primary text | `#20242C` | 15.55:1 | +102.5 | `#F2F3F5` | 15.36:1 | −98.4 |
| Secondary text | `#4F5663` | 7.38:1 | +85.7 | `#D1D5DE` | 11.60:1 | −79.4 |
| Tertiary text | `#646C79` | 5.30:1 | +76.4 | `#B8C0CE` | 9.31:1 | −66.7 |
| Accent / Open PR | `#2F55B7` | 6.77:1 | +82.9 | `#C4D6FF` | 11.70:1 | −80.0 |
| Positive / Safe to delete | `#246347` | 7.12:1 | +84.5 | `#B2E0C4` | 11.65:1 | −79.8 |
| Neutral / Prunable | `#596271` | 6.16:1 | +80.7 | `#D1D5DE` | 11.60:1 | −79.4 |
| Waiting / Not merged | `#79500F` | 7.08:1 | +84.3 | `#EAD0A8` | 11.44:1 | −78.6 |
| Protect / Keep | `#9F3442` | 6.90:1 | +83.0 | `#FFC1CB` | 11.16:1 | −77.1 |
| Decorative hairline | `#DFE2E7` | 1.30:1 | +14.8 | `#333945` | 1.47:1 | 0.0 |
| Control boundary | `#7B8391` | 3.82:1 | +65.7 | `#778398` | 4.45:1 | −34.3 |

The hairline is intentionally below functional contrast. Table structure is conveyed by spacing, headings, and semantic markup as well as separators. It carries no unique state information.

The selected background is the lowest-contrast background in the five-layer palette for these foregrounds. These are the measured worst cases for text outside the opaque chips:

| Foreground on selected fill | Light WCAG / Lc on `#E9EFFB` | Dark WCAG / Lc on `#242F49` |
| --- | ---: | ---: |
| Primary | 13.48:1 / +92.8 | 11.99:1 / −94.9 |
| Secondary | 6.40:1 / +76.0 | 9.05:1 / −76.0 |
| Tertiary | 4.59:1 / +66.7 | 7.27:1 / −63.2 |
| Accent | 5.86:1 / +73.2 | 9.13:1 / −76.5 |
| Positive | 6.17:1 / +74.7 | 9.10:1 / −76.3 |
| Neutral | 5.34:1 / +71.0 | 9.05:1 / −76.0 |
| Waiting | 6.13:1 / +74.6 | 8.93:1 / −75.1 |
| Protect | 5.98:1 / +73.2 | 8.71:1 / −73.7 |
| Control boundary | 3.31:1 / +56.0 | 3.48:1 / −30.8 |

`--on-accent` on `--accent` measures light **6.77:1 / −87.9 Lc**, dark **12.75:1 / +81.1 Lc**. Filled accent buttons are reserved for an empty state's single primary action; routine Sync and Copy controls remain neutral. Never reduce text opacity. Tertiary text is for short timestamps and supplemental metadata; instructions, blocker reasons, command strings, and transcript prose use primary or secondary text. Dark tertiary Lc is lower than body-text targets, so it must not carry sustained reading.

### State vocabulary and tone ownership

Each row below uses the corresponding measured palette pair above. Different meanings share a tone deliberately; the label and icon distinguish them. Worktree verdicts and PR workflow state remain separate concepts: an Open PR worktree can contain a PR with failing checks.

| State | Token | Icon and exact visible label | Light WCAG / Lc | Dark WCAG / Lc |
| --- | --- | --- | ---: | ---: |
| Safe to delete | `--verdict-safe` | Check in circle · Safe to delete | 7.12 / +84.5 | 11.65 / −79.8 |
| Prunable | `--verdict-prunable` | Dashed square · Prunable | 6.16 / +80.7 | 11.60 / −79.4 |
| Not merged | `--verdict-not-merged` | Open branch · Not merged | 7.08 / +84.3 | 11.44 / −78.6 |
| Open PR | `--verdict-open-pr` | Pull request · Open PR | 6.77 / +82.9 | 11.70 / −80.0 |
| Keep | `--verdict-keep` | Lock · Keep | 6.90 / +83.0 | 11.16 / −77.1 |
| Checks passing | `--pr-checks-passing` | Check · Checks passing | 7.12 / +84.5 | 11.65 / −79.8 |
| Checks failing | `--pr-checks-failing` | Cross in circle · Checks failing | 6.90 / +83.0 | 11.16 / −77.1 |
| Checks pending | `--pr-checks-pending` | Clock · Checks pending | 7.08 / +84.3 | 11.44 / −78.6 |
| Review required | `--pr-review-required` | Person + clock · Review required | 7.08 / +84.3 | 11.44 / −78.6 |
| Review approved | `--pr-review-approved` | Check · Approved | 7.12 / +84.5 | 11.65 / −79.8 |
| Review changes | `--pr-review-changes` | Comment + minus · Changes requested | 6.90 / +83.0 | 11.16 / −77.1 |
| Draft | `--pr-draft` | Dotted circle · Draft | 6.16 / +80.7 | 11.60 / −79.4 |
| Conflicts | `--pr-conflicts` | Crossed branches · Conflicts | 6.90 / +83.0 | 11.16 / −77.1 |

Ready to merge shares Positive; In merge queue shares Accent; Unknown and No checks reported share Neutral. PR blocker precedence and worktree safety stay in their existing server classifiers. The template consumes their state; JavaScript never re-derives safety or readiness from check counts. One status template owns label, icon, and role class. Counts use the same canonical state keys, never comparisons against translated display strings.

### Elevation and motion

Tables, KPI strips, filter chips, and phone cards have no shadow. Popovers use `--shadow-popover` plus a `1px --line` perimeter. The drawer uses `--shadow-drawer`, a neutral inner edge, and the theme backdrop; do not blur the content behind it.

Hover changes only background and foreground over `100ms`. Copy feedback crossfades over `120ms`, retains its label for `1600ms`, then restores the original label. Drawer entry is opacity `0 → 1` and translateX `16px → 0` over `180ms` with `--ease-enter`; exit reverses this over `140ms` with `--ease-exit`. Do not animate dimensions, counts, row sorting, or table reflow. Sync communicates progress in text with elapsed whole seconds; it has no perpetual spinner. Reduced motion removes transitions and translations; feedback text and its `1600ms` dwell remain.

## 3. Typography

**Use the system stack. Embed no font.** The UI ships `0` font files and adds `0` font bytes. A single OFL variable WOFF2 capped at `120KB` is a viable packaging format, but it adds an asset and a different reading texture without improving this utility. Native metrics and instant display matter more here. Do not add a font license, font preload, external request, subsetting step, or font build pipeline.

```css
:root {
  --font-sans: ui-sans-serif, system-ui, -apple-system,
    BlinkMacSystemFont, "Segoe UI", sans-serif;
  --font-mono: ui-monospace, "SFMono-Regular", Menlo, Monaco,
    Consolas, "Liberation Mono", monospace;
}
body { font-family: var(--font-sans); font-size: 14px; line-height: 20px; }
.numeric { font-variant-numeric: tabular-nums lining-nums; }
code, pre, .path, .branch { font-family: var(--font-mono); font-variant-ligatures: none; }
```

Use tabular numerals for KPIs, badges with counts, check totals, timestamps, sizes, and diffs. Right-align quantitative columns. Keep prose, titles, and repository names proportional. Use monospace for paths, branches, IDs, and copyable commands. Do not turn the entire dashboard into a terminal.

| Role | Size / line-height | Weight | Tracking | Color |
| --- | --- | ---: | ---: | --- |
| Wordmark | 20px / 24px | 650 | −0.4px | Primary |
| Page heading | 24px / 32px | 650 | −0.5px | Primary |
| KPI value | 28px / 32px | 600 | −0.5px | Primary; state tone only for a state count |
| Drawer heading | 20px / 28px | 650 | −0.3px | Primary |
| Repository/group heading | 16px / 24px | 600 | −0.1px | Primary |
| Tabs, row titles, buttons | 14px / 20px | 600 | 0px | Primary |
| Ordinary cells | 14px / 20px | 400 | 0px | Primary |
| Verdict/status label | 14px / 20px | 700 | 0px | State tone |
| Blocker reason | 13px / 20px | 500 | 0px | Secondary |
| Table heading, KPI label | 12px / 16px | 600 | 0px | Secondary |
| Supplemental metadata | 12px / 16px | 500 | 0px | Tertiary |
| Path or branch | 12px / 18px | 400 | 0px | Primary or Secondary |
| Command | 13px / 20px | 400 | 0px | Primary |
| Drawer transcript | 15px / 24px | 400 | 0px | Primary |
| Shortcut keycap | 11px / 16px | 500 | 0px | Secondary |
| Phone input | 16px / 24px | 400 | 0px | Primary |

Do not force font smoothing or convert text to images in the app. Use requested weights through the native stack; platforms with static faces select the nearest face. No all-caps table labels. Status labels never shrink to fit a column. Increase row height for wrapped evidence. At `200%` zoom, content reflows through the same breakpoints and remains operable.

## 4. Page-level direction

### Frame and header

Use a centered `1280px` maximum content width. Desktop outer padding is `32px` top, `24px` left/right, and `64px` bottom. At widths `640–1023px`, horizontal padding is `20px`. Below `640px`, it is `16px`, with `16px` top and `32px` bottom plus the bottom safe-area inset.

The brand header is `40px` high on desktop. Put the wordmark left and the sync cluster right. The cluster contains “Synced 2 min ago” in tertiary text, an outlined **Sync now** button `112 × 32px`, and a `32 × 32px` settings button with a `16px` gear. The settings popover is `240px` wide, padded `12px`, with Theme and Keyboard shortcuts controls. It opens below/right with `8px` clearance.

Under the brand header, leave `16px`, then the navigation row. Leave `24px` after navigation, then the page heading and a `4px` gap to its one-line scope description. Examples: “Worktrees” / “Under ~/Projects”; “Pull requests” / “Open PRs authored by you”; “Sessions” / “Claude Code and Codex sessions on this Mac”. Do not place operational diagnostics in the title region. Scan duration, API point usage, exact timestamps, and refresh schedules belong in a neutral “Sync details” disclosure below the scope description.

Neither the header nor tabs float over scrolling data. Let them scroll away. At viewport widths of `1328px` and above, only table column headers become sticky. This avoids stacked sticky bands consuming a laptop screen.

### Tabs and search

Keep exactly three routes and their order: **Worktrees** `/`, **Pull requests** `/prs`, **Sessions** `/sessions`. Render ordinary links in `<nav aria-label="Main">` with `aria-current="page"`. These are page navigation links, not an ARIA tab widget. Each is `44px` high with `12px` horizontal padding; gap `8px`; neutral `1px` baseline. Active: primary text, weight `600`, `2px` primary bottom line. Inactive: secondary text. Counts follow the label in tabular numerals with an `8px` gap.

Search is scoped to the selected tab. Place it below the KPI strip, on the same row as filters, aligned right. Desktop width `320px`, height `36px`, radius `6px`, inset `12px`, `16px` search icon, and a `20 × 20px` `/` keycap. Its visible label is “Search worktrees”, “Search pull requests”, or “Search sessions”; the accessible name matches. Search does not query or read new transcripts. It filters the fields already rendered into that tab's rows.

Org and tool filters are neutral outlined chips, `28px` high, horizontal padding `10px`, radius `999px`, gap `6px`. Selected chips include a `12px` check and `aria-pressed="true"`. Deselected chips lose the check and use secondary text; no strikethrough, no opacity reduction. **Show all** is a text button after the chips. Tool chips retain Claude Code, Codex, and Scheduled tasks. Filters and search share one visibility function per page, which also updates visible counts and hides empty groups.

When filtered, show “18 of 42 shown” and label the KPI strip “Filtered results”. Tab counts remain the complete snapshot totals. Worktree/PR aggregate values follow visible rows; machine disk capacity remains explicitly labeled “This Mac” and never changes with a filter. Persist organization/tool toggles in local storage and search text in session storage, separately per tab. Provide a visible **Clear filters** action whenever the result is filtered to zero.

### KPI strip

Place the strip `20px` below the scope area and `20px` above filters. Use one flat surface, `1px --line` top and bottom, padding `16px 0`, and no enclosing rounded boxes. Each cell has `16px` horizontal padding, value then label with `4px` gap. Separate cells using neutral `1px` vertical rules. Minimum cell height is `56px` inside the strip.

- Worktrees: four equal cells — Worktrees; On disk; Safe-to-delete size; Free on this Mac. The last cell's caption includes total capacity; the safe-size explanation states the logical-size limitation.
- Pull requests: five equal cells — Open; Ready to merge; Needs action; Waiting; Drafts. Ready uses Positive; Needs action uses Protect; Waiting uses Waiting; the other figures stay Primary. Needs action includes server-classified failing checks, conflicts, and changes requested without double counting; the Blocked state (branch protection) is a Waiting tone and counts there.
- Sessions: three equal cells — Sessions; Claude Code; Codex. A session count is not a running-agent count. Scheduled tasks remain a filter, not a second count of the same sessions.
- Before the first successful snapshot, display an em dash and “Waiting for first sync”. Zero is reserved for a successful result with no items.

### Grouping and table anatomy

Keep Worktrees' **By project**, **By folder**, and repository worktree groups, in that order. Put the two summary tables in native disclosures: By project initially open, By folder initially closed. Repository groups remain open and sorted by existing last-activity behavior; no new default sort by verdict. Group heading margin-top `32px`, heading-to-table gap `8px`. A group heading includes repository name, neutral item count, and optional upstream/default-branch metadata.

PRs stay sorted by last commit; Sessions stay sorted by last activity. Maintain the same column grid across all groups of a table kind. A table surface has a `1px --line` outer boundary and `10px` outer corners; internal separators are horizontal. No zebra striping. Header height `36px`, cell padding `8px 12px`; body rows minimum `64px`, cell padding `10px 12px`. Single-line aggregate rows are `40px` minimum. At viewport widths of `1328px` and above, leave wrapper overflow visible and stick header cells at `top: 0` with opaque inset fill; round the outer cells themselves instead of clipping the wrapper.

These column widths are exact at the desktop table width of `1280px`; widths include cell padding and sum to `1280px` for each table:

| Table | Columns in visual order, with widths |
| --- | --- |
| Worktrees | Worktree **208px**; Branch **184px**; Pull request **248px**; Verdict **192px**; Changes **80px**; Unpushed **88px**; Last active **144px**; Size **136px** |
| Pull requests | Repository **136px**; Pull request **280px**; Branch · local worktree **192px**; Blocker **200px**; Checks **104px**; Review **104px**; Last commit **88px**; Opened **80px**; Diff **96px** |
| Sessions | Tool **112px**; Session **464px**; Folder · branch **272px**; Last active **128px**; Size **120px**; Resume **184px** |
| By project / By folder | Repository or folder **288px**; Worktrees **88px**; Size **112px**; Safe-to-delete size **144px**; Safe to delete **160px**; Prunable **112px**; Not merged **136px**; Open PR **128px**; Keep **112px** |

At `640–1327px` viewport widths, retain the `1280px` table grid inside a labeled horizontal scroll region. Sticky first column: Worktree `208px`, Repository `136px`, Tool `112px`, summary name `288px`; keep its fill opaque and use a neutral right hairline. In this range, column headers scroll vertically with the document; do not create a nested vertical scroller to make them sticky. Put the horizontal scroll region in keyboard tab order with an accessible name and visible focus. Below `640px`, replace the table presentation with the card layout specified below; the page itself never scrolls horizontally.

Worktree rows show a basename then its parent folder; branch; PR number and title; verdict and full reason; modified/untracked counts; unpushed count; last activity with commit time beneath; logical size. Never truncate the verdict, reason, or nonzero protection count. PR rows show title and number, local linkage, the primary blocker and its explanation, then supporting checks/review data. Session rows show tool and kind, title and last-prompt preview, folder/branch, last activity/start time, size, and Copy resume.

Clip long paths and branches to a single line in desktop cells. PR and session titles take up to two lines. Add a keyboard/touch-accessible “Show full value” disclosure for clipped fields; `title` alone is insufficient. The disclosure uses the row's surface and wraps the complete value with `overflow-wrap: anywhere`. Do not put copyable commands in ellipsized text. Missing data is an em dash with accessible “Not available”; a measured zero remains `0`.

Use semantic table headers and captions on desktop. A session title is a real button that opens its drawer; clicking unused row space triggers the same action. Links and Copy buttons keep their own actions and never bubble into drawer opening. Worktree and PR rows are not giant links. Their explicit links, disclosures, and command actions provide navigation.

### Status anatomy

Verdict badges are compact labels on neutral surface: minimum height `24px`, padding `2px 6px`, radius `4px`, icon `14 × 14px`, icon-to-label gap `6px`, text `14px/20px` at weight `700`. Allow labels to wrap at spaces where necessary, increasing badge and row height; never truncate them. Do not use colored badge borders. PR blocker labels use the same template and geometry.

Secondary count rows use an `8px` solid dot or `14px` icon followed by explicit text: “7 passed”, “2 pending”, “1 failed”. Check and review evidence remains visible even when another blocker wins precedence. Icons use `1.5px` strokes on a `16px` grid with round caps; hide them from assistive technology when the adjacent text supplies their meaning. A dot is never the only status communication.

**No colored left-border stripes, `::before` rails, tinted inset bars, edge glows, or verdict-colored row backgrounds.** The only status color is in the icon, dot, label, or relevant figure. Keep uses a lock and its reason; red does not imply the user must delete or repair anything.

### Empty, loading, stale, and error states

Place state messages in a neutral surface with `1px --line` all around, `10px` radius, `24px` padding, a `20px` icon, and an `8px` title-to-body gap. Use left-aligned text. Height is content-driven with `120px` minimum. No illustrations, celebratory graphics, skeleton shimmer, or left accent stripe.

| Situation | Exact copy and action |
| --- | --- |
| First worktree scan | **Scanning worktrees…** “Results appear when the first scan finishes.” Show unavailable KPI values, not zero. |
| Successful empty worktree scan | **No linked worktrees found.** “Scanned {root}. Create a worktree, then sync again.” Action: Sync now. |
| Successful empty PR result | **No open pull requests.** “You're viewing open PRs authored by the signed-in GitHub account.” Action: Sync now. |
| Successful empty session result | **No sessions found.** “Claude Code and Codex sessions will appear after you use them on this Mac.” |
| No filtered matches | **No matches.** “Try another search or show all items.” Action: Clear filters. |
| Partial refresh failure | **Couldn't refresh {count} repositories.** “Showing their last successful results.” Actions: Sync now; Error details disclosure. Mark affected groups “Last successful sync {time}”. |
| PR refresh failure with cache | **Couldn't refresh pull requests.** “Showing results from {time}.” Action: Sync now. |
| No successful cache and failed sync | **Couldn't load results.** “No successful sync is available yet.” Action: Retry sync. Do not show an empty-success message. |
| Old snapshot | **Results are {age} old.** Display this once age exceeds `10 min`. Action: Sync now. Snapshot age is not proof of a connection failure. |
| Rate-limit pause | **GitHub refresh paused.** “Showing the last successful results.” Show a reset time only when supplied by the server; otherwise say “Waiting for API capacity.” |
| LAN session restriction | **Sessions are available on this Mac only.** “Open agentyard on the Mac that runs it to view transcripts and resume commands.” Keep the Sessions tab visible; render no transcript, preview, private count, or session search data. |
| Session folder deleted | **Folder deleted.** “Recreate {folder} before resuming this session.” Show in row and drawer. Copy resume stays available with this explanation. |

The source's existing server-side local-only rule controls session availability, including its explicit LAN opt-in. The design must not implement this restriction merely by hiding HTML. Never claim every feature is phone-accessible by default.

### Copy commands and Sync now

Cleanup commands remain inside each repository's disclosure, titled “Commands for {count} safe-to-delete worktrees”. Show one command per line, `13px/20px` mono, with a **Copy** button at the right. The command well uses inset fill, `6px` radius, `12px` padding, and horizontal scrolling for the exact shell string. Put “Commands are copied, never run here.” and the logical-size explanation above it. Do not add destructive confirmation dialogs to copy actions.

Use the existing server-generated remove and resume strings verbatim; the client must not reconstruct paths or quoting. Resume retains the correct recorded working directory and tool-specific command. One shared copy helper serves table rows, command disclosures, and the drawer.

Copy buttons are `120 × 32px` for **Copy resume**, `72 × 32px` for **Copy**. Width does not change on feedback. On successful copying show a `14px` check and **Copied** for `1600ms`, announce “Resume command copied” or “Command copied” through one polite live region, then restore the label. No toast and no layout movement.

On secure contexts, attempt the Clipboard API first. For `http://<name>.local`, preserve the existing textarea selection fallback. If copying fails, open a neutral command panel with the complete string in a visible read-only textarea, select it, and show **Select and copy this command.** Provide **Close** and restore focus afterward. On phones the text can wrap visually inside this fallback without changing the copied string. Never display Copied until the clipboard operation reports success.

Sync now uses the existing POST/status flow and the `30s` server cooldown. Idle: **Sync now**. In progress: **Syncing… 8s**, disabled, with `aria-busy="true"`; elapsed time is not announced every second. A recent-sync response says **Just synced** for `3000ms` and restores idle. Failure says **Sync failed** and presents a Retry action with the last successful snapshot intact. Honor a sync started on another device.

Keep the existing five-minute refresh cadence. Defer navigation/reload while search is focused, a drawer or full-value disclosure is open, or command text is selected. When sync completes during one of these interactions, show **Updated results ready** with an **Apply update** button; applying it reloads and preserves the tab/filter state. Otherwise restore the page's scroll position after refresh. One refresh coordinator owns periodic refresh and explicit Sync completion; remove conflicting meta-refresh behavior when implementing this direction.

### Session detail drawer

Replace the current inline expansion with a right-side native `<dialog>` presented modally. Desktop width `600px`, maximum `calc(100vw - 48px)`, height `100dvh`, fixed flush to the right. Square outer corners, `24px` internal padding, neutral inner edge, drawer shadow, and opaque surface. The underlying selected row receives `--bg-selected` and no rail.

Header: session title in `20px/28px`, tool/kind and last-activity metadata beneath, `32 × 32px` Close button top right. Then a full-width command well, **Copy resume**, folder/branch, linked PR, and the folder-deleted message when applicable. Allow long titles to wrap without displacing Close. Sticky header and command area remain inside the drawer; the transcript alone scrolls.

Fetch only the selected session through the existing detail endpoint. Show **Loading messages…** while waiting. Render the latest available `40` messages, retaining the product's limited transcript window. Show **Latest 40 messages** only when 40 messages are actually present; otherwise **Latest {count} messages**. Open at the latest message after content insertion without animation. Loading failures show **Couldn't load this session.** and a Retry button inside the drawer.

Transcript width is the drawer's inner width. Message gap `20px`; role/time row `12px/16px` above message text with `4px` gap. User messages use inset fill, `6px` radius, and `12px` padding. Assistant messages use the plain surface. Tool entries are neutral disclosures labeled “{count} tool calls”. Plain transcript text uses `white-space: pre-wrap` and `overflow-wrap: anywhere`; code uses monospace. Do not add a Markdown parser, syntax-highlighting dependency, or interpretation of transcript text as HTML.

Focus the drawer heading (`tabindex="-1"`) on open, label the dialog from that heading, keep Tab/Shift+Tab within it, make the background inert, close on Escape or Close, and restore focus to the session title button. Backdrop clicks do not close it. Global tab/sync shortcuts are suspended while a modal is open. These behaviors follow the [W3C modal dialog pattern](https://www.w3.org/WAI/ARIA/apg/patterns/dialog-modal/).

### Keyboard and accessibility

| Key | Behavior |
| --- | --- |
| `/` | Focus and select the current tab's search text. No action on the restricted Sessions notice. |
| `1` / `2` / `3` | Navigate to Worktrees / Pull requests / Sessions through their real links. |
| `g`, then `s` | Sync now if the second key arrives within `800ms` and sync is enabled. |
| `Escape` | Close the active drawer/popover; otherwise clear a focused search field. |
| `Tab` / `Shift+Tab` | Follow native visible-control order. |
| `Enter` / `Space` | Activate the focused button using native behavior. Enter follows links. |

Ignore global shortcuts inside inputs, textareas, selects, editable content, IME composition, held/repeating keys, and events with Ctrl/Alt/Meta. The `g s` sequence cancels on blur, Escape, another key, or timeout. Handle shortcuts in one function. Keep all actions available through labeled controls.

Settings includes **Keyboard shortcuts** with a checkbox, initially on, persisted under `agentyard.shortcuts`. Turning it off disables `/`, `1/2/3`, and `g s`; native keyboard behavior remains. This also covers speech-input users: character sequences require an off/remap mechanism. [W3C character-key guidance](https://www.w3.org/WAI/WCAG22/Understanding/character-key-shortcuts.html).

Add a Skip to content link. Use proper headings, labeled search, `time` elements with exact datetimes, explicit links, visible focus, and one polite announcement region. Do not announce every relative-time tick. Keep links underlined within explanatory prose; in dedicated link columns, their column heading and conventional link treatment provide context. Errors are announced once when newly produced, not on every repaint.

### Phones: below 640px

At `639px` and below, rows become stacked cards. Render the same row data and status/action partials into the alternate presentation; do not maintain an independent mobile classifier or color mapping. The hidden presentation must use `display: none` so its controls leave the focus/accessibility tree, and IDs must remain unique. Preserve desktop table semantics rather than forcing table elements into misleading ARIA roles.

- Header: first row has the `24px` mark and `20px` wordmark left, settings target `44 × 44px` right. A second `44px` row, `8px` below, places the sync timestamp left and Sync button `112 × 44px` right. This fits a `320px` viewport without compressing the identity. All icon-button hit targets and actionable row controls are at least `44 × 44px` with `8px` between separate actions.
- Navigation: three equal-width columns, `44px` high, `4px` horizontal padding, `13px/20px` type. Hide tab count badges below `640px`; counts remain in the heading/KPI area. Keep all three names visible at `320px` viewport width.
- KPI strip: two columns with `16px` row gap; remove vertical separators and use neutral horizontal group separation. Each cell has `12px` padding. An odd final KPI spans both columns. Values stay `28px`.
- Search becomes full-width and `44px` high, with `16px` input text. Put it above wrapping filter chips. Chips use `44px` hit targets; their visible pill remains `28px` high. Hide shortcut keycaps.
- Card: surface fill, `1px --line` perimeter, `10px` radius, `16px` padding, `12px` between cards. Content gap `8px`. No fixed height. Primary text `15px/22px`; secondary `13px/20px`; paths `12px/18px`. Group headings retain `16px/24px`.
- Worktree card: basename, then verdict and full reason, then path/branch, PR, and a two-column labeled metric grid for Changes, Unpushed, Last active, and Size. Nonzero changes and unpushed values stay visible without expanding the card. Cleanup commands remain in the group disclosure below its cards.
- PR card: title and PR number, blocker and full reason, repository/branch/local worktree, checks and review, then Last commit / Opened / Diff in a wrapping metadata row. Title and blocker wrap fully.
- Session card: title button, tool/kind and latest prompt, folder/branch and any deleted-folder warning, then last activity/size and a full-width `44px`-high Copy resume button. Its row click behavior never absorbs the Copy action.
- Summary cards retain every aggregate value. Show group name and Worktrees/Size/Safe-to-delete size first; put the five labeled verdict counts in a native “Verdict counts” disclosure. Do not omit Prunable or Keep to save space.
- Drawer becomes full-screen: width `100vw`, maximum width `100vw`, `100dvh`, no radius/shadow, `16px` padding plus safe-area insets. Close and Copy are `44px` high. Transcript text remains `15px/24px`.
- All paths and metadata wrap using `overflow-wrap: anywhere`; command wells keep horizontal scrolling confined to the command itself. There is no page-level horizontal overflow, hover-only help, swipe-only action, or bottom toolbar covering content.

### What makes it current

The refinement comes from a stable spatial system, clear blocker precedence, typography that carries real information, immediate keyboard access, honest freshness, and an uninterrupted transition between laptop rows and phone cards. Content stays in place during copying and deferred refresh. A drawer reveals depth without losing the selected session. Every status is understandable in grayscale. The single page feels fast because the server renders useful HTML immediately and small native interactions progressively enhance it.

Keep palette/state markup, clipboard handling, filter visibility/counts, theme selection, drawer lifecycle, and refresh scheduling behind one owner each. Add no client-side router, virtual table, animation library, icon font, or generated CSS toolchain. Inline SVG provides the mark and the small icon vocabulary. Marketing raster assets never enter the dashboard's critical path. Static assets are served locally with the Go standard library and embedded in the binary where needed; the ordinary Go build is the only build step.

## 5. Asset list and production prompts

These are production specifications, not generated deliverables. Create only this brief in this task. Future artwork uses invented demonstration labels exclusively: repository `yard-demo`, branches `agent/parser`, `agent/docs`, and `agent/tests`, PRs `#42` and `#43`, and session “Refine parser errors”. No screenshots, user paths, actual PR titles, transcripts, customer names, or tokens.

All raster exports are sRGB PNG, lossless, without EXIF or other personal metadata. All SVGs have a viewBox, no scripts, no external resources, and no embedded font. Final brand geometry comes from the canonical path in section 1. Generation supplies a visual draft; exact paths and typography are composited/exported deterministically afterward. No generated logo approximation becomes the favicon. The prompts below are self-contained and ready to paste.

### App icon — light

- **Purpose:** macOS launcher/bookmark and project identity. This artwork does not imply a new native wrapper or Dock process.
- **File:** `assets/app-icon-light.png`.
- **Size:** `1024 × 1024px`, RGBA, transparent outside the icon silhouette.
- **Construction:** centered macOS-style continuous squircle inside `[64,64]–[960,960]`; flat fill `#F7F7F5`; `2px` inside edge `#DFE2E7`; no exterior shadow. Define the final contour with `M288 64H736C889 64 960 135 960 288V736C960 889 889 960 736 960H288C135 960 64 889 64 736V288C64 135 135 64 288 64Z`. Place the `16 × 16` mark in a `576 × 576px` box at `(224,206)` in `#2F55B7`; the `18px` upward box adjustment centers its visible geometry. No text.

**Prompt:**

> Create a 1024x1024 macOS-style app icon for agentyard, a quiet developer utility. Transparent canvas outside a single centered continuous-corner squircle occupying x64–960 and y64–960. Squircle fill #F7F7F5, 2px inner edge #DFE2E7, perfectly flat, no shadow or texture. Inside place one original cobalt #2F55B7 geometric mark: three separate horizontal square-ended tracks, each 2 units thick on a 16x16 grid, starting at x2 at y2, y7, y12; their right ends are x8, x11, x14. At the last 2 units of each track extend downward by 1 unit, producing short square terminal stops. Use the exact three silhouettes M2 2H8V5H6V4H2Z; M2 7H11V10H9V9H2Z; M2 12H14V15H12V14H2Z. Place their 16-unit grid in a 576px square at x224 y206. Crisp orthogonal geometry. No letters, gradients, glass, bevels, perspective, circles, nodes, left-edge stripes, or existing brand symbols.

### App icon — dark

- **Purpose:** dark desktop/bookmark variant of the identical icon.
- **File:** `assets/app-icon-dark.png`.
- **Size:** `1024 × 1024px`, RGBA, same silhouette, placement, and transparent exterior as light.
- **Construction:** squircle `#191C23`, `2px` inside edge `#333945`, mark `#C4D6FF`. Same contour and mark boxes as light. No glow or exterior shadow.

**Prompt:**

> Create a 1024x1024 dark macOS-style agentyard app icon. Transparent exterior. A continuous squircle occupies x64–960 and y64–960, flat #191C23 with a 2px inside edge #333945. Place a single pale blue #C4D6FF mark in a 576x576 box at x224 y206. The mark is exactly these three filled shapes on a 16x16 grid: M2 2H8V5H6V4H2Z; M2 7H11V10H9V9H2Z; M2 12H14V15H12V14H2Z. They are three independent parallel horizontal tracks of 2-unit thickness with unequal lengths and 1-unit downward terminal stops. Preserve square corners and open gaps. No text, lighting effects, glow, texture, gradient, perspective, decorative borders, left-edge colored stripes, or existing logo resemblance. Match the light icon's shape and placement exactly.

### README hero/banner

- **Purpose:** the repository's first visual explanation; ship one light composition for consistent legibility inside GitHub's image frame.
- **File:** `assets/readme-hero.png`.
- **Size:** `1600 × 560px`, RGB, opaque `#F7F7F5`. Target file size at most `300KB`.
- **Layout:** left text safe margin `64px`; right artwork top/bottom margin `48px`. Left column `(64,64)`, width `536px`; right panel `(664,48)`, size `872 × 464px`. Left: `40px` mark beside `32px/40px` wordmark; headline at `(64,136)`, `44px/50px`, weight `650`, on exactly two lines: “Your coding agents,” / “one calm yard.” Supporting text at `(64,268)`, `20px/28px`, width `520px`; footer descriptor at `(64,432)`, `16px/24px`: “One Go binary. One calm page.” Panel fill `#FFFFFF`, perimeter `1px #DFE2E7`, radius `12px`, no shadow. Use a composed illustration of the prescribed UI, not a capture.
- **Panel content:** horizontal inset `24px`. Tabs at `y=72`, `18px/24px`; KPI band at `y=112`, height `48px`, numbers `28px/32px`. Three `72px` demonstration rows start at `y=176`, `248`, `320`, with `16px/22px` text: `agent/docs` — Safe to delete; `agent/parser` — Open PR; `agent/tests` — Keep. Session strip at `y=416`, height `72px`: “Refine parser errors” / “Copy resume”. This leaves `24px` below the strip. Use exact semantic colors, dots/icons/labels, and no status rails.
- **Alt text:** “agentyard: worktrees, pull requests, and agent sessions in one calm dashboard.”

**Prompt:**

> Design a precise 1600x560 README banner for an open-source developer tool named agentyard. Opaque background #F7F7F5, 64px left text margin and 48px top/bottom artwork margin, flat editorial composition. Left column x64 y64 width536. Place an original geometric mark of three disconnected horizontal square tracks with short downward end stops and lengths 6, 9, 12 on a 16-unit grid, beside lowercase agentyard in graphite #20242C. At x64 y136 set the headline exactly “Your coding agents,” then “one calm yard.” in 44px system sans, 50px line height, weight650. At x64 y268 set supporting text: “Every worktree, pull request, and agent session on one page.” Footer at x64 y432: “One Go binary. One calm page.” Right panel x664 y48 width872 height464, white, 12px corners, 1px #DFE2E7 perimeter, no shadow. Show a fictional compact interface with Worktrees, Pull requests, Sessions navigation at y72, a KPI band at y112 height48, and exactly three 72px rows starting at y176, y248, y320: agent/docs — Safe to delete; agent/parser — Open PR; agent/tests — Keep. State ink: green #246347, blue #2F55B7, red #9F3442, neutral ink #20242C. At y416 show a 72px neutral strip with “Refine parser errors” and a “Copy resume” button. Use labels and small icons for status. No real data, screenshots, brand copies, colored left-edge stripes, gradients, perspective, glass, mascots, glow, or fake terminal windows. Preserve ample blank space and crisp alignment. Typography will be finished as exact typesetting after generation.

### GitHub social preview

- **Purpose:** repository link previews and social sharing.
- **File:** `assets/github-social.png`.
- **Size:** `1280 × 640px`, RGB, opaque `#111318`. Target file size at most `250KB`.
- **Layout:** all essential content inside `(80,80)–(1200,560)`. Lockup at `(80,80)`: `48px` mark, `16px` gap, `40px/48px` wordmark, `#F2F3F5`. Headline at `(80,184)`, width `1000px`, `56px/64px`, weight `650`, on exactly two lines: “Your coding agents,” / “one calm yard.” Supporting line at `(80,344)`, `24px/32px`, `#D1D5DE`: “Every worktree, PR, and session. One Go binary.” At `(80,456)`, three `336 × 72px` neutral panels with `24px` gaps, fill `#191C23`, radius `8px`, perimeter `1px #333945`; labels Worktrees / Pull requests / Sessions in `24px/32px`, primary ink, and one `20px` pale-blue icon each. No miniature table text.
- **Alt text:** “agentyard — Your coding agents, one calm yard. Worktrees, pull requests, and sessions.”

**Prompt:**

> Create a 1280x640 GitHub repository social preview for agentyard. Opaque charcoal background #111318. Keep essential content inside an 80px safe margin. At x80 y80 place an original three-track orthogonal yard mark, 48px, beside lowercase “agentyard” in 40px system sans with a 16px gap, color #F2F3F5. The mark uses three disconnected horizontal lanes of lengths 6, 9, 12 and thickness2 on a 16-unit grid, each with a tiny square downward end stop; no diagonal stripes or existing brand symbol. At x80 y184 set exactly two headline lines, “Your coding agents,” and “one calm yard.”, 56px type, 64px line height, weight650, #F2F3F5. At x80 y344 set “Every worktree, PR, and session. One Go binary.” in 24px #D1D5DE. At y456 place three flat 336x72 panels at x80, x440, x800 with 8px corners, fill #191C23, 1px #333945 perimeter; label them “Worktrees”, “Pull requests”, “Sessions” in 24px primary text and add one small #C4D6FF outline icon per panel. Calm, precise, strongly readable at thumbnail size. No screenshot, fine-print table, glow, gradient, glass, 3D, mascot, logos of other products, or colored left-edge status stripes. Finish all text with exact typesetting after generation.

### Adaptive SVG favicon

- **Purpose:** browser tab at `16px` and scalable bookmark favicon.
- **File:** `assets/favicon.svg`.
- **Size:** `viewBox="0 0 16 16"`, transparent, target at most `1KB` uncompressed.
- **Construction:** the exact path from section 1. Fill `#2F55B7` by default; an internal `prefers-color-scheme: dark` rule changes it to `#C4D6FF`. No wordmark, squircle, filter, stroke, or external CSS. Browser theme determines favicon color independently of the page preference.

**Prompt:**

> Produce a flat vector-style favicon study for agentyard on a transparent 16x16 canvas. It contains only three filled square-corner shapes with exact paths M2 2H8V5H6V4H2Z; M2 7H11V10H9V9H2Z; M2 12H14V15H12V14H2Z. These are separate parallel tracks with increasing lengths and short downward end stops. Use #2F55B7; the dark-background alternate uses #C4D6FF. No surrounding tile, text, gradient, antialiased blur, shadow, added connectors, colored edge stripe, or existing brand symbol. Deliver a clean geometry reference; the final SVG will be constructed directly from the supplied paths rather than traced from raster output.

### PNG favicon fallback

- **Purpose:** browsers/contexts that need a fixed raster icon.
- **File:** `assets/favicon-32.png`.
- **Size:** exactly `32 × 32px`, RGB, opaque, at most `2KB`.
- **Construction:** `#F7F7F5` full canvas; canonical path scaled by exactly `2`, filled `#2F55B7`. No squircle or shadow. The fixed opaque tile preserves contrast regardless of browser chrome. Rasterize directly at `32px`; do not downsample the app icon.

**Prompt:**

> Produce a 32x32 pixel favicon for agentyard. Solid #F7F7F5 square canvas. Draw the exact three filled paths from a 16x16 grid scaled by 2: M2 2H8V5H6V4H2Z; M2 7H11V10H9V9H2Z; M2 12H14V15H12V14H2Z. Fill all three #2F55B7. Edges land on integer pixels, all corners square, no extra connector. No text, shadow, squircle, gradient, glow, existing brand symbol, or colored left-edge decoration. This is a reference for deterministic raster export from the canonical SVG, not an invitation to redraw the mark.

Asset acceptance is exact: verify the mark at actual `16px` and `32px`, icon silhouette at `64px`, README at `800 × 280px`, and social preview at `640 × 320px`. Correct all generated text during final composition. Reject extra strokes, merged tracks, invented UI labels, clipped safe areas, or colored left-edge state decoration. Keep source marketing artwork outside the dashboard render path; embed only the favicon and other assets the running page actually serves.
