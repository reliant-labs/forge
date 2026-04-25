import React, { useState } from "react";

type AlertVariant = "info" | "success" | "warning" | "error";

interface AlertBannerProps {
  variant?: AlertVariant;
  title?: string;
  message: string;
  dismissible?: boolean;
  onDismiss?: () => void;
  action?: { label: string; onClick: () => void };
}

const variantConfig: Record<AlertVariant, { bg: string; border: string; icon: string; title: string; text: string }> = {
  info: { bg: "bg-blue-50", border: "border-blue-200", icon: "text-blue-500", title: "text-blue-800", text: "text-blue-700" },
  success: { bg: "bg-green-50", border: "border-green-200", icon: "text-green-500", title: "text-green-800", text: "text-green-700" },
  warning: { bg: "bg-yellow-50", border: "border-yellow-200", icon: "text-yellow-500", title: "text-yellow-800", text: "text-yellow-700" },
  error: { bg: "bg-red-50", border: "border-red-200", icon: "text-red-500", title: "text-red-800", text: "text-red-700" },
};

const icons: Record<AlertVariant, React.ReactNode> = {
  info: (
    <svg className="h-5 w-5" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" d="M11.25 11.25l.041-.02a.75.75 0 011.063.852l-.708 2.836a.75.75 0 001.063.853l.041-.021M21 12a9 9 0 11-18 0 9 9 0 0118 0zm-9-3.75h.008v.008H12V8.25z" />
    </svg>
  ),
  success: (
    <svg className="h-5 w-5" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" d="M9 12.75L11.25 15 15 9.75M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
    </svg>
  ),
  warning: (
    <svg className="h-5 w-5" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v3.75m-9.303 3.376c-.866 1.5.217 3.374 1.948 3.374h14.71c1.73 0 2.813-1.874 1.948-3.374L13.949 3.378c-.866-1.5-3.032-1.5-3.898 0L2.697 16.126zM12 15.75h.007v.008H12v-.008z" />
    </svg>
  ),
  error: (
    <svg className="h-5 w-5" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
      <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v3.75m9-.75a9 9 0 11-18 0 9 9 0 0118 0zm-9 3.75h.008v.008H12v-.008z" />
    </svg>
  ),
};

export default function AlertBanner({ variant = "info", title, message, dismissible, onDismiss, action }: AlertBannerProps) {
  const [visible, setVisible] = useState(true);
  const config = variantConfig[variant];

  if (!visible) return null;

  function handleDismiss() {
    setVisible(false);
    onDismiss?.();
  }

  return (
    <div className={`rounded-lg border p-4 ${config.bg} ${config.border}`} role="alert">
      <div className="flex items-start gap-3">
        <div className={`flex-shrink-0 ${config.icon}`}>{icons[variant]}</div>
        <div className="flex-1 min-w-0">
          {title && <p className={`text-sm font-semibold ${config.title}`}>{title}</p>}
          <p className={`text-sm ${config.text} ${title ? "mt-1" : ""}`}>{message}</p>
          {action && (
            <button
              onClick={action.onClick}
              className={`mt-2 text-sm font-medium underline ${config.title} hover:opacity-80`}
            >
              {action.label}
            </button>
          )}
        </div>
        {dismissible && (
          <button onClick={handleDismiss} className={`flex-shrink-0 ${config.icon} hover:opacity-70`}>
            <svg className="h-4 w-4" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
            </svg>
          </button>
        )}
      </div>
    </div>
  );
}
