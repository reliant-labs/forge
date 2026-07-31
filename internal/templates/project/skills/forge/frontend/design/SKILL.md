---
name: design
description: Visual-design discipline for forge frontends — brief the work before building (never invent an aesthetic without user input), declare the design system before coding, lean on the component library, use restrained color/type systems, never hand-draw complex SVGs, and verify visually before declaring done.
---

# Frontend Visual Design

**What something looks like** — the design-quality layer above the engineering patterns in `frontend/patterns`. Load it whenever you are creating UI the user will see.

## Brief the work before you build

For any non-trivial visual work — a new screen, a new component shape, anything described as "make a UI for X" — ask a structured brief BEFORE writing code. Skipping this is the single biggest source of "looks AI-generated" output.

Cover, at minimum:

- **Reference material** — Existing design system? A site, app, or Figma to match? A screenshot? **Never invent an aesthetic without user input.** For greenfield or unbranded work, do not start designing until you have one of: a reference, a brand, or an explicit "decide for me" from the user. Starting without any of the three is how you get slop.
- **Audience and tone** — Internal admin tool? Consumer landing page? Developer docs? The default visual answers diverge.
- **Variations** — Variations are a deliverable, not a courtesy. When the brief is open-ended ("design an X") or the user wants options, first ask which dimension to vary — layout, visual treatment, copy, or interaction — then produce **2-3 clearly-labeled options** side by side with stable ids (`A` / `B` / `C`) the user can reference in follow-ups ("go with B but use A's header"). Render them with the `variation_grid` layout (`component_library(action="get", name="variation_grid")`) so each option gets a labeled artboard. Do not hand-roll a comparison div.
- **Fidelity** — Sketch / wireframe / hi-fi / production code? Don't ship pixel-polish when the user wanted a wireframe.
- **Content** — Is the copy real or placeholder? If placeholder, will real copy be longer/shorter? Many layouts break when real copy arrives.

Skip the brief only for small tweaks ("change this color", "add this button") or when the user has clearly given you everything.

### The "decide for me" fallback

When the user explicitly delegates the aesthetic, don't reach for taste — derive the system from the checkable rules in **Color discipline** and **Type discipline** below: 1-3 fonts as a known-good pairing (one display + one body); neutrals at chroma ≤ 0.02 with hue borrowed from the accent; 0-2 accents sharing lightness and chroma, varying only hue. Then declare the resulting system (see "Declare the system") before writing any component code.

## Recreating or extending existing UI? Read the source first

When you touch an existing product surface, every element you produce is a recreation by default — of that project's visual vocabulary, not a fresh invention. Before writing code:

- Read the project's component library (`frontends/<app>/src/components/ui/`) and its theme — the tokens in `globals.css` and the Tailwind config.
- Read 2-3 neighboring pages or components that do something similar to what you're building.
- Note the vocabulary you must match, not just the colors: copywriting tone, hover/click/focus states, shadow depth, corner radii, spacing density.

A new screen that matches the palette but ignores the project's hover states and shadow language still reads as foreign. If you haven't read the neighboring code, you haven't started.

## Reach for the component library first

`component_library` ships 60+ production-ready React/TypeScript components. Search it BEFORE hand-rolling anything visual:

```
component_library(action="search", category="diagrams")
component_library(action="search", tag="dashboard")
component_library(action="get", name="quadrant_chart")
```

No `component_library` tool in your environment? The binary carries the same registry: `forge component list`, `forge component search <keyword>`, `forge component install <name>` (categories: `layouts`, `charts`, `diagrams`, `deck`, `ui`).

Charts and diagrams handle their own coordinate math — pass data, get pixels. If the library has a component that's 80% right, install it and adapt; do not start from a blank file.

For diagrams specifically, see `diagrams`.

## Declare the system, then follow it

After the brief (or the source-reading pass), state the design system out loud in your response BEFORE building — consistency is only checkable if it's declared. Name, concretely:

- **Type**: the 1-3 font families and which scale steps you'll use where.
- **Spacing**: the scale (Tailwind's default steps count) — no ad-hoc pixel values.
- **Backgrounds**: 1-2 background colors per surface, max, each named.
- **Layout pattern per content type**: e.g. tables for records, cards for summaries, split-pane for master/detail — so the same content type never gets two treatments.

Then follow it. Any element that deviates from the declared system needs a reason you could say out loud; if you can't, it's drift.

## Color discipline

- **Use design tokens.** Tailwind theme tokens (`bg-card`, `text-muted-foreground`, `border-border`), shadcn semantic names (`primary` / `secondary` / `destructive` / `muted`), or CSS variables defined in `globals.css`. Never invent raw hex colors when a token expresses the intent.
- **When you must define new colors, use oklch — `%` lightness, `deg` hue.** `oklch(72% 0.12 250deg)` is a sky blue. Pick a lightness and chroma, vary hue across the palette. The bare-number spelling `oklch(0.72 0.12 250)` is valid CSS but red under the scaffold's stylelint (`lightness-notation` / `hue-degree-notation`) — one error per token. `npm run lint:styles:fix` rewrites a file into the passing form.
- **No pure `#fff` / `#000`.** Backgrounds and text want subtly-toned near-whites and near-blacks — chroma ≤ 0.02, hue borrowed from the accent. Saturation creeping into greys is the #1 tell of LLM-generated palettes.
- **Zero to two accents, max.** A primary, optionally one secondary at the same lightness and chroma with a different hue — sharing L and C and varying only H keeps the pair harmonious by construction. Diagrams full of seven different colors are slop.

## Type discipline

- **1-3 fonts, max.** A body font and optionally a display font is plenty. More than that is noise.
- **Avoid the AI defaults when YOU pick the font.** Inter, Roboto, Arial, and Fraunces scream "model picked this." **A font the project already declares is not your pick** — forge's Next.js scaffold loads Inter through `next/font` and exposes it as `--font-sans` in `globals.css`; that is the declared system (the neutral default for an app with no brief yet), so keep it unless the user hands you a brand or a reference. Same for any existing font token: use it, don't introduce a new family on a whim. Rebranding swaps the `--font-sans` token and the `layout.tsx` loader, never per-component `font-*` classes.
- **Set a type scale, don't pick sizes ad-hoc.** Tailwind's `text-xs / sm / base / lg / xl / 2xl / 3xl` is a scale — use it. `text-[17px]` once-off is fine for a tight constraint, but `text-[17px]` / `text-[19px]` / `text-[23px]` across one screen is not.
- **Scale floors — check the numbers, don't eyeball.** Body copy: ≥ 14px. Touch targets: ≥ 44px square. Slide / hero-canvas text: ≥ 24px on a 1920×1080 canvas. Print: ≥ 12pt. Anything below a floor is a defect, not a style choice.

## Never hand-draw complex SVGs

The model is bad at SVG. The line is **primitive shapes**:

- **OK to hand-write:** rectangles, circles, lines, diamonds, simple icons from `lucide-react`, single-path checkmarks.
- **Don't hand-write:** logos, illustrations, multi-shape diagrams, organic curves, anything you would describe as "a small drawing of X."

For anything in the "don't" bucket, use a **placeholder slot** instead — sized to the real asset's dimensions, with a subtle fill (e.g. faint diagonal stripes or `bg-muted/30`) and a monospace label naming what belongs there:

```tsx
<div className="flex aspect-video items-center justify-center rounded-lg border border-dashed border-border bg-muted/30">
  <span className="font-mono text-xs text-muted-foreground">[product shot — 16:9]</span>
</div>
```

Then ASK the user for the real asset, or accept that the placeholder ships. A clearly-marked, correctly-sized slot is honest; a hand-drawn-by-LLM logo is not.

## Layout primitives

- **Flex/grid + `gap`, not inline siblings.** For any row or group of sibling elements (buttons, chips, icons, cards, nav items, toolbars), use `flex` or `grid` with `gap-*` for spacing. Don't rely on whitespace text nodes or per-element margins. Explicit-gap layouts survive direct-manipulation edits (drag-reorder, duplicate, delete) cleanly; inline flow doesn't.
- **`text-wrap: pretty` / `balance`** for headings and short paragraphs — kills orphan lines that look bad in small cards.
- **Container queries** for components that render at different widths in different contexts. Media queries are for the page; container queries are for the component.

## Anti-slop tropes to avoid

A blocklist, not a mood board — each item is checkable in a screenshot:

- **Aggressive gradient backgrounds.** High-saturation or rainbow gradients as page/hero backgrounds, or on buttons / cards / headings. Subtle low-contrast surface gradients are the ceiling.
- **Containers with `rounded-2xl` + a left-border accent stripe.** The signature "AI dashboard card." Use it sparingly or not at all.
- **Emoji** anywhere in the UI (✨ ✅ 🚀 🎯) unless the brand demonstrably uses them.
- **Data slop** — decorative stats ("+12%", "99.9%"), meters, and icon rows that inform nothing, invented to fill space. Numbers must be real, or omitted.
- **Every section needing an icon.** Icons earn their place when they aid scanning; decorative icons are visual noise.
- **"Powered by" / "Built with" filler footers** on internal tools.

## No filler content

Every element earns its place — one thousand no's for every yes; less is more. An empty-feeling section is a **layout problem, not a content problem**: solve it with composition, whitespace, or by cutting the section — never by inventing copy or fake data to fill it. **Ask before adding sections, pages, or copy that the user didn't request.**

## Verify visually before declaring done

Code that type-checks and tests that pass do not prove a UI looks right. Before reporting visual work complete:

1. Launch the app and load the page you changed (use the `run` skill if available, or the explicit dev-server command).
2. Take a screenshot. Look at it. Compare to the brief.
3. Check at least one other viewport width (mobile if the design is desktop-first, or vice versa).
4. Run the overflow probe below against the changed surface and read the report.
5. Check for overflow / text wrap / alignment regressions in the surrounding area, not just the changed component.

If you cannot run the app, say so explicitly — do not claim visual success based on the diff alone.

### Overflow probe

Headlines that escape their card, captions that wrap into a third line, columns that scroll horizontally on tablet — the diff never shows these. This snippet finds them. Run it in the browser via Chrome DevTools MCP (`mcp__chrome-devtools__evaluate_script`) against the root selector of the surface you changed:

```js
function auditOverflow(rootSelector) {
  const root = document.querySelector(rootSelector);
  if (!root) return { error: `no element matches ${rootSelector}` };

  const cssPath = (el) => {
    const parts = [];
    while (el && el.nodeType === 1 && parts.length < 6) {
      let s = el.tagName.toLowerCase();
      if (el.id) { s += "#" + el.id; parts.unshift(s); break; }
      if (el.className && typeof el.className === "string") {
        const cls = el.className.trim().split(/\s+/).slice(0, 2).join(".");
        if (cls) s += "." + cls;
      }
      parts.unshift(s);
      el = el.parentElement;
    }
    return parts.join(" > ");
  };

  const issues = [];
  const all = [root, ...root.querySelectorAll("*")];

  // (a) element's own content overflows its box
  for (const el of all) {
    if (el.scrollWidth - el.clientWidth > 1) {
      issues.push({ type: "scroll-x", sel: cssPath(el), over: el.scrollWidth - el.clientWidth, text: (el.innerText || "").slice(0, 80) });
    }
    if (el.scrollHeight - el.clientHeight > 1) {
      issues.push({ type: "scroll-y", sel: cssPath(el), over: el.scrollHeight - el.clientHeight, text: (el.innerText || "").slice(0, 80) });
    }
  }

  // (b) descendant's rect escapes a non-`overflow:visible` ancestor's rect
  function walk(el, parent) {
    if (parent) {
      const cr = el.getBoundingClientRect();
      const pr = parent.getBoundingClientRect();
      const cs = getComputedStyle(parent);
      const clipped = cs.overflow !== "visible" || cs.overflowX !== "visible" || cs.overflowY !== "visible";
      const escapes =
        cr.right > pr.right + 0.5 || cr.left < pr.left - 0.5 ||
        cr.bottom > pr.bottom + 0.5 || cr.top < pr.top - 0.5;
      if (escapes && !clipped) {
        issues.push({ type: "escapes-parent", sel: cssPath(el), parent: cssPath(parent), text: (el.innerText || el.tagName).slice(0, 80) });
      }
    }
    for (const c of el.children) walk(c, el);
  }
  walk(root, null);

  return { rootSelector, count: issues.length, issues: issues.slice(0, 40) };
}

auditOverflow("main");  // or a tighter selector for the surface you changed
```

Two classes of finding:

- `scroll-x` / `scroll-y` — an element's own content exceeds its box. Common cause: a long word/URL, a fixed-width child, or a table that doesn't shrink.
- `escapes-parent` — a child's bounding rect extends past an ancestor that isn't clipping. Common cause: absolute positioning gone wrong, negative margins, or a heading that overflows because its container shrank.

Run it at multiple viewports (resize via `mcp__chrome-devtools__resize_page` first) — a layout that's clean at 1440px often falls apart at 768px or 375px.
