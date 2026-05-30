---
name: design
description: Visual-design discipline for forge frontends — brief the work before building, lean on the component library, use restrained color/type systems, never hand-draw complex SVGs, and verify visually before declaring done.
---

# Frontend Visual Design

This skill is about **what something looks like** — the design quality layer above the engineering patterns in `[[patterns]]`. Load it whenever you are creating UI that the user will see, not just wiring up state or APIs.

## Brief the work before you build

For any non-trivial visual work — a new screen, a new component shape, anything described as "make a UI for X" — ask a structured brief BEFORE writing code. Skipping this is the single biggest source of "looks AI-generated" output.

Cover, at minimum:

- **Reference material** — Existing design system? A site, app, or Figma to match? A screenshot? If none, ask the user to attach one OR explicitly approve a divergent direction. Building without a reference is how you get slop.
- **Audience and tone** — Internal admin tool? Consumer landing page? Developer docs? The default visual answers diverge.
- **Variations** — When the brief is open-ended ("design an X"), default to **2-3 variations** the user can compare, not one chosen direction. Ask which axes to vary (layout? visual treatment? interaction? copy?). Render them with the `variation_grid` layout (`component_library(action="get", name="variation_grid")`) so each option gets a labeled artboard and the user can scan them side by side. Do not hand-roll a comparison div.
- **Fidelity** — Sketch / wireframe / hi-fi / production code? Don't ship pixel-polish when the user wanted a wireframe.
- **Content** — Is the copy real or placeholder? If placeholder, will real copy be longer/shorter? Many layouts break when real copy arrives.

Skip the brief only for small tweaks ("change this color", "add this button") or when the user has clearly given you everything.

## Reach for the component library first

`component_library` ships 60+ production-ready React/TypeScript components. Search it BEFORE hand-rolling anything visual:

```
component_library(action="search", category="diagrams")
component_library(action="search", tag="dashboard")
component_library(action="get", name="quadrant_chart")
```

If you don't have the `component_library` tool in your environment, read the component files directly from disk: they live under `<forge-repo>/components/components/<category>/<name>.tsx` (categories: `layouts`, `charts`, `diagrams`, `deck`, `ui`). The registry in `<forge-repo>/components/library.go` lists every component, its category, and tags.

Charts and diagrams handle their own coordinate math — pass data, get pixels. Hand-rolling a chart from scratch will be worse and take longer. If the library has a component that's 80% right, install it and adapt; do not start from a blank file.

For diagrams specifically, see `[[diagrams]]`.

## Color discipline

- **Use design tokens.** Tailwind theme tokens (`bg-card`, `text-muted-foreground`, `border-border`), shadcn semantic names (`primary` / `secondary` / `destructive` / `muted`), or CSS variables defined in `globals.css`. Never invent raw hex colors when a token expresses the intent.
- **When you must define new colors, use oklch.** `oklch(0.72 0.12 250)` is a sky blue. Pick a lightness and chroma, vary hue across the palette. Keep all "neutral" whites/blacks at chroma ≤ 0.02; saturation creeping into greys is the #1 tell of LLM-generated palettes.
- **One or two accents, max.** Pick a primary accent. Optionally one secondary at the same lightness and chroma, with a different hue. Stop there. Diagrams full of seven different colors are slop.
- **Avoid aggressive gradients as decoration.** Subtle, low-contrast gradients are fine for surfaces; rainbow or high-saturation gradients on buttons / cards / headings are not.

## Type discipline

- **1-3 fonts, max.** A body font and optionally a display font is plenty. More than that is noise.
- **Pull from the existing system.** If the project already has font tokens in `globals.css` or `tailwind.config`, use those. Don't introduce a new font family on a whim.
- **Avoid the AI defaults.** Inter, Roboto, Arial, and Fraunces are the four fonts that scream "model picked this." Use them only if the project's design system already does.
- **Set a type scale, don't pick sizes ad-hoc.** Tailwind's `text-xs / sm / base / lg / xl / 2xl / 3xl` is a scale — use it. `text-[17px]` once-off is fine for a tight constraint, but `text-[17px]` / `text-[19px]` / `text-[23px]` across one screen is not.
- **Hit-targets and minimums.** Body copy: ≥ 14px. Mobile buttons / tap targets: ≥ 44px square. Slide / presentation type: ≥ 24px.

## Never hand-draw complex SVGs

The model is bad at SVG. Concretely:

- **OK to hand-write:** rectangles, circles, lines, simple icons from `lucide-react`, single-path checkmarks.
- **Don't hand-write:** logos, illustrations, multi-shape diagrams, organic curves, anything you would describe as "a small drawing of X."

For anything in the "don't" bucket, use a **placeholder slot** instead:

```tsx
<div className="flex aspect-video items-center justify-center rounded-lg border border-dashed border-border bg-muted/30">
  <span className="font-mono text-xs text-muted-foreground">[product shot — 16:9]</span>
</div>
```

Then ASK the user for the asset, or accept that the placeholder ships. A clearly-marked slot is honest; a hand-drawn-by-LLM logo is embarrassing.

## Layout primitives

- **Flex/grid + `gap`, not inline siblings.** For any row or group of sibling elements (buttons, chips, icons, cards, nav items, toolbars), use `flex` or `grid` with `gap-*` for spacing. Don't rely on whitespace text nodes or per-element margins. Explicit-gap layouts survive direct-manipulation edits (drag-reorder, duplicate, delete) cleanly; inline flow doesn't.
- **`text-wrap: pretty` / `balance`** for headings and short paragraphs — kills orphan lines that look bad in small cards.
- **Container queries** for components that render at different widths in different contexts. Media queries are for the page; container queries are for the component.

## Anti-slop tropes to avoid

These read as "AI-generated" to any designer who looks at the output:

- **Containers with `rounded-2xl` + a left-border accent stripe.** The signature "AI dashboard card." Use it sparingly or not at all.
- **Heavy use of decorative emoji** as section markers (✨ ✅ 🚀 🎯). Skip unless the brand uses emoji.
- **Decorative gradient meshes / glow blobs** behind hero content. Once in a while is fine; on every card is slop.
- **Stat-card rows with arbitrary "+12%" metrics** invented to fill space. Numbers must be real, or omitted.
- **"Powered by" / "Built with" filler footers** on internal tools.
- **Every section needing an icon**. Icons earn their place when they aid scanning; decorative icons are visual noise.

## No filler content

Every element earns its place. If a section feels empty, that's a design problem to solve with layout and composition — not by inventing copy or fake data to fill it. **Ask before adding sections, pages, or copy that the user didn't request.**

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

## Rules

- For non-trivial visual work, ask a design brief BEFORE coding.
- Search `component_library` before hand-rolling anything visual.
- Use design tokens; if you must add colors, use oklch with low chroma for neutrals.
- 1-3 fonts, off the AI-default list unless the project already uses them.
- Use placeholders + ask for assets — never hand-draw a logo or illustration in SVG.
- Flex/grid + `gap`, never bare inline siblings.
- Default to 2-3 variations for open-ended design asks.
- Verify visually — screenshot, don't trust the diff.
