"use client";

import React, { useState } from "react";

interface NavItem {
  label: string;
  href?: string;
  icon?: React.ReactNode;
  active?: boolean;
  section?: string;
}

interface UserInfo {
  name: string;
  email?: string;
  avatar?: React.ReactNode;
}

interface SidebarLayoutProps {
  brand: React.ReactNode;
  navItems: NavItem[];
  user?: UserInfo;
  children: React.ReactNode;
  headerContent?: React.ReactNode;
}

export default function SidebarLayout({
  brand,
  navItems,
  user,
  children,
  headerContent,
}: SidebarLayoutProps) {
  const [collapsed, setCollapsed] = useState(false);

  const sections = new Map<string, NavItem[]>();
  for (const item of navItems) {
    const key = item.section ?? "";
    if (!sections.has(key)) sections.set(key, []);
    sections.get(key)!.push(item);
  }

  return (
    <div className="flex h-screen overflow-hidden bg-gray-50">
      {/* Sidebar */}
      <aside
        className={`flex flex-col border-r border-gray-200 bg-white transition-all duration-200 ${
          collapsed ? "w-16" : "w-64"
        }`}
      >
        {/* Brand */}
        <div className="flex h-16 shrink-0 items-center justify-between border-b border-gray-200 px-4">
          {!collapsed && <div className="truncate">{brand}</div>}
          <button
            onClick={() => setCollapsed(!collapsed)}
            className="flex h-8 w-8 items-center justify-center rounded-lg text-gray-400 transition hover:bg-gray-100 hover:text-gray-600"
            aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
          >
            <svg className="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              {collapsed ? (
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M13 5l7 7-7 7M5 5l7 7-7 7" />
              ) : (
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M11 19l-7-7 7-7m8 14l-7-7 7-7" />
              )}
            </svg>
          </button>
        </div>

        {/* Navigation */}
        <nav className="flex-1 overflow-y-auto p-3">
          {Array.from(sections.entries()).map(([section, items], si) => (
            <div key={si} className={si > 0 ? "mt-6" : ""}>
              {section && !collapsed && (
                <p className="mb-2 px-3 text-xs font-semibold uppercase tracking-wider text-gray-400">
                  {section}
                </p>
              )}
              {si > 0 && collapsed && (
                <div className="mx-auto my-2 h-px w-6 bg-gray-200" />
              )}
              <ul className="space-y-1">
                {items.map((item, i) => (
                  <li key={i}>
                    <a
                      href={item.href ?? "#"}
                      className={`flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition ${
                        item.active
                          ? "bg-blue-50 text-blue-700"
                          : "text-gray-600 hover:bg-gray-100 hover:text-gray-900"
                      } ${collapsed ? "justify-center" : ""}`}
                      title={collapsed ? item.label : undefined}
                    >
                      {item.icon && (
                        <span className="h-5 w-5 shrink-0">{item.icon}</span>
                      )}
                      {!collapsed && <span className="truncate">{item.label}</span>}
                    </a>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </nav>

        {/* User Area */}
        {user && (
          <div className="shrink-0 border-t border-gray-200 p-3">
            <div
              className={`flex items-center gap-3 rounded-lg px-3 py-2 ${
                collapsed ? "justify-center" : ""
              }`}
            >
              <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-gray-200 text-sm font-semibold text-gray-600">
                {user.avatar ?? user.name.charAt(0).toUpperCase()}
              </div>
              {!collapsed && (
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium text-gray-900">
                    {user.name}
                  </p>
                  {user.email && (
                    <p className="truncate text-xs text-gray-500">
                      {user.email}
                    </p>
                  )}
                </div>
              )}
            </div>
          </div>
        )}
      </aside>

      {/* Main */}
      <div className="flex flex-1 flex-col overflow-hidden">
        {/* Header Bar */}
        <header className="flex h-16 shrink-0 items-center border-b border-gray-200 bg-white px-6">
          {headerContent}
        </header>

        {/* Content */}
        <main className="flex-1 overflow-y-auto p-6">{children}</main>
      </div>
    </div>
  );
}