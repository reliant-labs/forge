/**
 * Forge Event Bus — typed pub/sub for imperative cross-cutting actions.
 *
 * Default events are provided. Extend by merging into the EventMap interface:
 *
 * ```typescript
 * // In your code:
 * declare module "@/lib/events" {
 *   interface EventMap {
 *     "editor:focusNode": { nodeId: string };
 *     "workflow:runRequested": { workflowId: string };
 *   }
 * }
 * ```
 */

export interface ToastEvent {
  message: string;
  variant?: "success" | "error" | "warning" | "info";
  duration?: number;
}

// Default event map — users extend this via TS declaration merging
export interface EventMap {
  "toast:show": ToastEvent;
  "toast:dismiss": { id?: string };
  "navigate": { path: string };
  "auth:expired": undefined;
  "auth:login": undefined;
  "auth:logout": undefined;
}

type EventHandler<T> = (payload: T) => void;

export class EventBus {
  private listeners = new Map<string, Set<EventHandler<unknown>>>();
  private devMode: boolean;

  constructor(devMode = false) {
    this.devMode = devMode;
  }

  emit<K extends keyof EventMap>(
    event: K,
    ...args: EventMap[K] extends undefined ? [] : [EventMap[K]]
  ): void {
    if (this.devMode) {
      console.debug(`[event] ${String(event)}`, args[0] ?? "");
    }
    const handlers = this.listeners.get(event as string);
    if (handlers) {
      for (const handler of handlers) {
        handler(args[0]);
      }
    }
  }

  on<K extends keyof EventMap>(
    event: K,
    handler: EventHandler<EventMap[K]>,
  ): () => void {
    const key = event as string;
    if (!this.listeners.has(key)) {
      this.listeners.set(key, new Set());
    }
    this.listeners.get(key)!.add(handler as EventHandler<unknown>);

    // Return unsubscribe function
    return () => {
      this.listeners.get(key)?.delete(handler as EventHandler<unknown>);
    };
  }

  off<K extends keyof EventMap>(
    event: K,
    handler: EventHandler<EventMap[K]>,
  ): void {
    this.listeners.get(event as string)?.delete(handler as EventHandler<unknown>);
  }
}

// Singleton — initialized in Providers
let _bus: EventBus | null = null;

export function initEventBus(devMode = false): EventBus {
  if (!_bus) {
    _bus = new EventBus(devMode);
  }
  return _bus;
}

export function getEventBus(): EventBus {
  if (!_bus) {
    throw new Error(
      "EventBus not initialized — call initEventBus() in Providers first",
    );
  }
  return _bus;
}
