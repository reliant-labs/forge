import React from "react";

interface Breadcrumb {
  label: string;
  href?: string;
}

interface Action {
  label: string;
  onClick?: () => void;
  href?: string;
  variant?: "primary" | "secondary" | "danger";
  icon?: React.ReactNode;
}

interface PageHeaderProps {
  title: string;
  subtitle?: string;
  breadcrumbs?: Breadcrumb[];
  actions?: Action[];
}

export default function PageHeader({ title, subtitle, breadcrumbs = [], actions = [] }: PageHeaderProps) {
  const variantStyles: Record<string, string> = {
    primary: "bg-blue-600 text-white hover:bg-blue-700 shadow-sm",
    secondary: "border border-gray-300 bg-white text-gray-700 hover:bg-gray-50 shadow-sm",
    danger: "border border-red-200 bg-white text-red-600 hover:bg-red-50 shadow-sm",
  };

  return (
    <div className="mb-6">
      {breadcrumbs.length > 0 && (
        <nav className="mb-3 flex items-center gap-1.5 text-sm text-gray-500">
          {breadcrumbs.map((crumb, i) => (
            <React.Fragment key={i}>
              {i > 0 && (
                <svg className="h-3.5 w-3.5 text-gray-400" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor">
                  <path strokeLinecap="round" strokeLinejoin="round" d="M8.25 4.5l7.5 7.5-7.5 7.5" />
                </svg>
              )}
              {crumb.href ? (
                <a href={crumb.href} className="hover:text-gray-700">
                  {crumb.label}
                </a>
              ) : (
                <span className="text-gray-900 font-medium">{crumb.label}</span>
              )}
            </React.Fragment>
          ))}
        </nav>
      )}
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold text-gray-900">{title}</h1>
          {subtitle && <p className="mt-1 text-sm text-gray-500">{subtitle}</p>}
        </div>
        {actions.length > 0 && (
          <div className="flex items-center gap-2">
            {actions.map((action, i) => {
              const cls = `inline-flex items-center gap-1.5 rounded-lg px-4 py-2 text-sm font-medium focus:outline-none focus:ring-2 focus:ring-blue-500 focus:ring-offset-2 ${variantStyles[action.variant ?? "secondary"]}`;
              if (action.href) {
                return (
                  <a key={i} href={action.href} className={cls}>
                    {action.icon}
                    {action.label}
                  </a>
                );
              }
              return (
                <button key={i} onClick={action.onClick} className={cls}>
                  {action.icon}
                  {action.label}
                </button>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
