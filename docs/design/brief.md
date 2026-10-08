# agentyard interface

The running interface is a local developer workspace for worktrees, pull requests, and agent sessions. `page.html` is the canonical owner of its theme tokens, components, SVG sprite, and client interactions. This brief describes the design rather than maintaining a second copy of those definitions.

## Visual system

- Graphite surfaces in dark mode; pale neutral surfaces in light mode.
- Violet identifies navigation, focus, and selected controls. Mint, amber, and rose carry server-derived status. No colored left-border callouts.
- The existing three-track agentyard mark appears in a compact violet tile. Page headings are factual: Worktrees, Pull requests, Sessions.
- System sans for interface text; system monospace for paths, branches, commands, and compact chart values. No remote fonts or runtime asset requests.
- A fixed sidebar provides desktop navigation. Below 640px, it becomes a horizontal navigation header and data tables become cards.
- Desktop collection controls share a 36px height. Phone filter options and row actions provide larger touch targets; the filter panel is anchored above the bottom edge with scrollable options.
- No slogans, decorative product copy, or Demo badges. Development screenshots must still use synthetic data exclusively.

## Worktrees

Four metrics summarize count, logical disk usage, safe-to-delete size, and free disk space. A native interactive treemap maps measured worktree sizes to proportional areas, with optional repository grouping. Zero-size entries remain in the table and are explicitly excluded from chart area.

Verdict colors use the same CSS roles as table badges. Selecting a tile shows the underlying branch, path, size, verdict, and reason. The detail action focuses the corresponding visible table row or phone card. A repository breakdown displays each project's share of the currently filtered size.

Search and multi-select verdict/repository filters update the map, metrics, project summaries, cleanup command visibility, and rows in one pass. The chart consumes canonical table rows; it never reclassifies safety. Cleanup remains copy-only.

## Pull requests

A repository workspace header shows the current scope, PR total, attention count, and latest activity. Its searchable picker moves between all repositories, an organization, or a single repository. The status dropdown and shared Filter menu use the same option catalog and selection state. Selections use OR within a field and AND between fields; option counts exclude that option's own field. Active selections can be removed individually, and Reset all clears them with search.

The left overview uses inline `organization / repository` headings with aligned attention, count, and activity columns. Organizations and repositories follow the chosen activity or name sort. Collapsed repositories retain their metadata; expanded groups show two PR previews with a Show more action. Search and filtering lift this preview limit. A GitHub action is always available on each row.

The overview follows normal document scrolling. A separate sticky reader shows the selected PR's description, changed files, checks, reviews, branch, and local worktree. Long checks use a disclosure in the reader rather than stretching list rows. Below 900px, the reader replaces the list and offers a Back to pull requests action with focus restoration. Below 640px, existing mobile navigation remains unchanged.

Selected, hovered, and keyboard-focused rows have quiet rounded surfaces. A single boundary renderer suppresses separators on both sides of those surfaces, using the actually visible rows so filtering and previews do not leave orphan dividers. Long titles wrap; metadata stays aligned. Descriptions render a small Markdown subset as text and safe elements, never raw HTML.

## Sessions

The collection opens directly onto the session list. Claude uses its sunburst mark in terracotta; Codex uses the OpenAI mark in mint. One `tool-icon` template supplies filters, rows, and drawer metadata. Agent cells use the icon with an accessible name and hover title. Session source and linked PR remain visible alongside the title and prompt preview.

Agent quick views are icon-only, with accessible names and hover titles. Source labels combine an icon, text, and a quiet tone. The Filter menu supports source and folder selection alongside agents and search. Every route shares one option catalog, selected-value state, persistence, and visibility controller. Sorting supports recent/oldest activity and name; worktrees and sessions also support largest first.

The drawer retains focus management, Escape dismissal, selectable transcript text, and the existing copy-resume action. Recent file activity never implies that an agent process is running.

## Implementation and verification

One Go binary, standard library only, one embedded HTML template, no frontend framework or build step. Native SVG, CSS, and JavaScript implement the charts and interaction. Server verdict and PR classifiers remain authoritative. All filters use the same client-side state and visibility owner. Their controls precede the content they filter. Quick views, menu checkboxes, and active tokens read the same catalog; no visible agent name is repeated beside its mark.

Theme selection remains System / Light / Dark. Reduced motion disables transitions. Keyboard navigation, accessible control names, native disclosures, and clipboard fallback remain available.

Verify using `agentyard demo`, `go test -race ./...`, and `go vet ./...`. Browser verification covers chart area accuracy, scoped filters, empty results, selection, session drawers, both themes, and phone/tablet/desktop overflow. Save screenshots only from synthetic demo data.
