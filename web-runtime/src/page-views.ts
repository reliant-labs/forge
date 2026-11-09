// Part of @reliantlabs/forge-web-runtime — the web twin of forge/pkg.
//
// Page views as product events: one `page_viewed {route}` per route the user
// lands on, so a funnel or a "top pages" panel over product events needs no
// code in the app.
//
// ── Why not the SDK's own routeChange spans ──────────────────────────────
//
// HyperDX already records `documentLoad` and `routeChange` spans, but those
// are traces: they carry the full page URL, and the product-event view reads
// only `custom-action` spans (what track() records). A page view that a
// funnel can use has to be a product event, with a route that is safe to keep.
//
// ── What `route` is ──────────────────────────────────────────────────────
//
// The PATHNAME, never the URL: the query string and fragment are where tokens,
// OAuth codes and search terms live, and they are cut. The frontend's mount
// prefix (basePath) is stripped, so `/admin/billing` under basePath `/admin`
// is `/billing`, the route the app's own router knows.
//
// Every other segment is kept only when it is a plain route word: lowercase
// letters, joined by single `-` or `_` (`settings`, `sign-in`, `order_items`),
// at most MAX_ROUTE_WORD_CHARS long, or a version (`v2`). Anything else — a
// number, a UUID, a ULID, a short base64url id (`aB3xY_9k`), `jane.doe`,
// `~jane`, an encoded or mixed-case segment, anything with a digit — becomes
// `:id`. The rule keeps what a route table is made of and drops what an id
// is made of, rather than recognizing id formats one by one: an id format
// nobody listed would otherwise leave the browser. So `/orders/8812` and
// `/orders/aB3xY_9k` are both `/orders/:id`. The one thing a lexical rule
// cannot tell from a route word is an all-lowercase-letters value
// (`/users/jane`); an app with such routes names them itself with
// `routeName`. forge's generated pages keep ids in the query
// (`/orders/view?id=…`) already; this is for the routes an app writes itself.
//
// `routeName` output is held to the same rule (it is commonly built from the
// pathname), except that it may also use parameter placeholders: `:name`,
// `[name]`, `[...name]`, `[[...name]]` and `*`.
//
// ── How a route change is seen ───────────────────────────────────────────
//
// Next.js's App Router and React Router both navigate with
// history.pushState / replaceState and hear back/forward as popstate. Where
// the browser has the Navigation API, its `currententrychange` event reports
// all of those (and intercepted navigations) without touching `history`;
// elsewhere pushState and replaceState are wrapped — each wrapper calls
// through untouched, then looks at the new pathname — and popstate is
// listened to. The current route is reported once at install.
//
// Consecutive identical routes are one view: a replaceState that only moves
// the query, the hash or scroll state, or a re-render that pushes the same
// path, reports nothing.
//
// Dependency-free like track.ts: nothing here imports the SDK, and nothing
// runs until initBrowserTelemetry installs it (the consent gate), which also
// removes it again on disableTelemetry().
import { normalizeBasePath } from "./basepath.js";
import { isTelemetryDisabled, track } from "./track.js";

/** The product event a page view is recorded as. */
export const PAGE_VIEWED_EVENT = "page_viewed";

/** What an id-looking path segment becomes. */
export const ROUTE_ID_PLACEHOLDER = ":id";

/** Longest route reported; a longer one is cut. */
export const MAX_ROUTE_CHARS = 200;

/** Longest path segment kept as a route word. */
export const MAX_ROUTE_WORD_CHARS = 32;

// A route word: lowercase letters, single `-` / `_` joins. No digits: a digit
// is the commonest mark of an id.
const ROUTE_WORD_RE = /^[a-z]+(?:[-_][a-z]+)*$/;
// An API-style version segment.
const VERSION_RE = /^v\d{1,2}$/;
// What a route TABLE writes for a parameter, accepted from routeName only.
const PLACEHOLDER_RE =
  /^(?::[A-Za-z_][A-Za-z0-9_]{0,31}|\[{1,2}(?:\.\.\.)?[A-Za-z_][A-Za-z0-9_]{0,31}\]{1,2}|\*)$/;

function keepSegment(segment: string, placeholders: boolean): boolean {
  if (segment.length > MAX_ROUTE_WORD_CHARS) {
    return false;
  }
  return (
    ROUTE_WORD_RE.test(segment) ||
    VERSION_RE.test(segment) ||
    (placeholders && PLACEHOLDER_RE.test(segment))
  );
}

// The path without query and fragment, every segment either kept or `:id`,
// no empty segments, capped. Always starts with `/`.
function scrubRoute(path: string, placeholders: boolean): string {
  const cut = path.search(/[?#]/);
  const bare = cut === -1 ? path : path.slice(0, cut);
  const segments = bare
    .split("/")
    .filter((s) => s !== "")
    .map((s) => (keepSegment(s, placeholders) ? s : ROUTE_ID_PLACEHOLDER));
  return `/${segments.join("/")}`.slice(0, MAX_ROUTE_CHARS);
}

/**
 * The route a pathname is reported as: without query or fragment, without
 * the basePath prefix, with every segment that is not a plain route word
 * replaced by `:id` (see the header), no trailing slash and no empty
 * segments. Always starts with `/`. Pure; exported so an app can name its
 * own events' routes the same way.
 *
 *   normalizeRoute("/orders/8812?tab=items")        -> "/orders/:id"
 *   normalizeRoute("/admin/billing/", "/admin")     -> "/billing"
 *   normalizeRoute("/share/aB3xY_9k/edit")          -> "/share/:id/edit"
 */
export function normalizeRoute(pathname: string, basePath = ""): string {
  let path = typeof pathname === "string" ? pathname : "";
  const cut = path.search(/[?#]/);
  if (cut !== -1) {
    path = path.slice(0, cut);
  }
  const base = basePath.startsWith("/") ? normalizeBasePath(basePath) : "";
  if (base && (path === base || path.startsWith(`${base}/`))) {
    path = path.slice(base.length);
  }
  return scrubRoute(path, false);
}

/** How installPageViews names and reports routes. */
export interface PageViewOptions {
  /**
   * The frontend's mount prefix (`/admin`), stripped from every route. A
   * value that does not start with `/` (Vite's relative `./`) is ignored.
   */
  basePath?: string;
  /**
   * Names the route for a pathname (query and fragment already cut, basePath
   * still on), replacing normalizeRoute — an app with a route table maps
   * `/users/jane` to `/users/[id]` itself. Return null or "" to report
   * nothing for that pathname. The name is scrubbed like a pathname before
   * it is sent (query and fragment cut, every segment not a route word or a
   * parameter placeholder replaced by `:id`), so a name built from the
   * pathname cannot carry an id out.
   */
  routeName?: (pathname: string) => string | null | undefined;
}

/** What telemetry.ts installs: the route watch, with or without the events. */
export interface RouteWatchOptions extends PageViewOptions {
  /** Record `page_viewed {route}` for each new route. */
  pageViews: boolean;
  /**
   * Called with each new route and the one before it (null at install),
   * after its page view is recorded. Never throws into the app.
   */
  onRoute?: (route: string, previous: string | null) => void;
}

/** The slice of the Navigation API used here. */
interface NavigationLike {
  currentEntry?: { url?: string | null } | null;
  addEventListener(type: string, listener: () => void): void;
  removeEventListener(type: string, listener: () => void): void;
}

type HistoryMethod = History["pushState"];

/**
 * Watch the page's route and report each new one: a `page_viewed` event when
 * `pageViews` is on, and `onRoute` either way. Reports the current route at
 * once. Returns the function that removes every hook it installed; a wrapper
 * someone else has since wrapped stays in place, inert. A no-op without a
 * DOM. Called by initBrowserTelemetry; an app has no reason to call it.
 */
export function installPageViews(options: RouteWatchOptions): () => void {
  if (typeof window === "undefined" || isTelemetryDisabled()) {
    return () => {};
  }
  const basePath = options.basePath ?? "";
  let last: string | null = null;
  let active = true;

  const routeOf = (pathname: string): string | null => {
    if (options.routeName) {
      const cut = pathname.search(/[?#]/);
      const named = options.routeName(
        cut === -1 ? pathname : pathname.slice(0, cut),
      );
      return typeof named === "string" && named !== ""
        ? scrubRoute(named, true)
        : null;
    }
    return normalizeRoute(pathname, basePath);
  };

  const report = (pathname: string | undefined) => {
    if (!active || isTelemetryDisabled() || typeof pathname !== "string") {
      return;
    }
    try {
      const route = routeOf(pathname);
      if (route === null || route === last) {
        return;
      }
      const previous = last;
      last = route;
      if (options.pageViews) {
        track(PAGE_VIEWED_EVENT, { route });
      }
      options.onRoute?.(route, previous);
    } catch {
      // Telemetry must never break the app.
    }
  };

  const nav = navigationApi();
  const stops: Array<() => void> = [];
  if (nav) {
    const onEntry = () => report(entryPathname(nav) ?? currentPathname());
    nav.addEventListener("currententrychange", onEntry);
    stops.push(() => nav.removeEventListener("currententrychange", onEntry));
  } else {
    const history = window.history as History | undefined;
    if (history) {
      for (const method of ["pushState", "replaceState"] as const) {
        const original = history[method];
        if (typeof original !== "function") {
          continue;
        }
        const wrapped: HistoryMethod = function (this: History, ...args) {
          const result = original.apply(this, args);
          report(currentPathname());
          return result;
        };
        history[method] = wrapped;
        stops.push(() => {
          if (history[method] === wrapped) {
            history[method] = original;
          }
        });
      }
    }
    if (typeof window.addEventListener === "function") {
      const onPop = () => report(currentPathname());
      window.addEventListener("popstate", onPop);
      stops.push(() => window.removeEventListener?.("popstate", onPop));
    }
  }

  report(currentPathname());

  return () => {
    active = false;
    for (const stop of stops) {
      try {
        stop();
      } catch {
        // Telemetry must never break the app.
      }
    }
  };
}

function navigationApi(): NavigationLike | null {
  const nav = (window as unknown as { navigation?: NavigationLike }).navigation;
  return nav && typeof nav.addEventListener === "function" ? nav : null;
}

function entryPathname(nav: NavigationLike): string | undefined {
  const url = nav.currentEntry?.url;
  if (!url) {
    return undefined;
  }
  try {
    return new URL(url).pathname;
  } catch {
    return undefined;
  }
}

function currentPathname(): string | undefined {
  const path = window.location?.pathname;
  return typeof path === "string" ? path : undefined;
}
