/**
 * Toast Notification Component
 *
 * This component is controlled via props (toasts[] + onDismiss).
 *
 * Forge event bus integration example:
 *
 *   import { useEvent, useEmit } from "@/lib/event-context";
 *   import ToastNotification from "@/components/ui/toast_notification";
 *
 *   function ToastContainer() {
 *     const [toasts, setToasts] = useState<Toast[]>([]);
 *
 *     useEvent("toast:show", (payload) => {
 *       const id = Math.random().toString(36).slice(2);
 *       setToasts((prev) => [...prev, { id, ...payload }]);
 *     });
 *
 *     useEvent("toast:dismiss", (payload) => {
 *       if (payload?.id) {
 *         setToasts((prev) => prev.filter((t) => t.id !== payload.id));
 *       } else {
 *         setToasts([]);
 *       }
 *     });
 *
 *     return (
 *       <ToastNotification
 *         toasts={toasts}
 *         onDismiss={(id) => setToasts((prev) => prev.filter((t) => t.id !== id))}
 *       />
 *     );
 *   }
 *
 *   // To show a toast from anywhere:
 *   const emit = useEmit();
 *   emit("toast:show", { message: "Saved!", variant: "success" });
 */
import React, { useEffect, useState } from "react";

type ToastVariant = "success" | "error" | "warning" | "info";

interface Toast {
  id: string;
  message: string;
  variant?: ToastVariant;
  duration?: number;
}

interface ToastNotificationProps {
  toasts: Toast[];
  onDismiss: (id: string) => void;
  position?: "top-right" | "top-left" | "bottom-right" | "bottom-left";
}

const variantConfig: Record<ToastVariant, { bg: string; icon: string; border: string }> = {
  success: { bg: "bg-green-50", icon: "text-green-500", border: "border-green-200" },
  error: { bg: "bg-red-50", icon: "text-red-500", border: "border-red-200" },
  warning: { bg: "bg-yellow-50", icon: "text-yellow-500", border: "border-yellow-200" },
  info: { bg: "bg-blue-50", icon: "text-blue-500", border: "border-blue-200" },
};

const icons: Record<ToastVariant, React.ReactNode> = {
  success: (
    <svg className="h-5 w-5" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" d="M9 12.75L11.25 15 15 9.75M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
    </svg>
  ),
  error: (
    <svg className="h-5 w-5" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v3.75m9-.75a9 9 0 11-18 0 9 9 0 0118 0zm-9 3.75h.008v.008H12v-.008z" />
    </svg>
  ),
  warning: (
    <svg className="h-5 w-5" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v3.75m-9.303 3.376c-.866 1.5.217 3.374 1.948 3.374h14.71c1.73 0 2.813-1.874 1.948-3.374L13.949 3.378c-.866-1.5-3.032-1.5-3.898 0L2.697 16.126zM12 15.75h.007v.008H12v-.008z" />
    </svg>
  ),
  info: (
    <svg className="h-5 w-5" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" d="M11.25 11.25l.041-.02a.75.75 0 011.063.852l-.708 2.836a.75.75 0 001.063.853l.041-.021M21 12a9 9 0 11-18 0 9 9 0 0118 0zm-9-3.75h.008v.008H12V8.25z" />
    </svg>
  ),
};

function ToastItem({ toast, onDismiss }: { toast: Toast; onDismiss: (id: string) => void }) {
  const [visible, setVisible] = useState(false);
  const variant = toast.variant ?? "info";
  const config = variantConfig[variant];

  useEffect(() => {
    requestAnimationFrame(() => setVisible(true));
    const timer = setTimeout(() => onDismiss(toast.id), toast.duration ?? 5000);
    return () => clearTimeout(timer);
  }, [toast.id, toast.duration, onDismiss]);

  return (
    <div
      className={`pointer-events-auto flex w-80 items-start gap-3 rounded-lg border p-4 shadow-lg transition-all duration-300 ${config.bg} ${config.border} ${visible ? "translate-x-0 opacity-100" : "translate-x-4 opacity-0"}`}
    >
      <div className={`flex-shrink-0 ${config.icon}`}>{icons[variant]}</div>
      <p className="flex-1 text-sm text-gray-800">{toast.message}</p>
      <button onClick={() => onDismiss(toast.id)} className="flex-shrink-0 text-gray-400 hover:text-gray-600">
        <svg className="h-4 w-4" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
          <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
        </svg>
      </button>
    </div>
  );
}

const positionStyles: Record<string, string> = {
  "top-right": "top-4 right-4",
  "top-left": "top-4 left-4",
  "bottom-right": "bottom-4 right-4",
  "bottom-left": "bottom-4 left-4",
};

export default function ToastNotification({ toasts, onDismiss, position = "top-right" }: ToastNotificationProps) {
  return (
    <div className={`fixed z-50 flex flex-col gap-2 ${positionStyles[position]}`} aria-live="polite">
      {toasts.map((toast) => (
        <ToastItem key={toast.id} toast={toast} onDismiss={onDismiss} />
      ))}
    </div>
  );
}