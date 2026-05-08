import React from "react";

interface AvatarProps {
  src?: string;
  alt?: string;
  name?: string;
  size?: "xs" | "sm" | "md" | "lg" | "xl";
  status?: "online" | "offline" | "busy" | "away";
}

const sizeStyles: Record<string, { container: string; text: string; status: string }> = {
  xs: { container: "h-6 w-6", text: "text-[10px]", status: "h-1.5 w-1.5 ring-1" },
  sm: { container: "h-8 w-8", text: "text-xs", status: "h-2 w-2 ring-[1.5px]" },
  md: { container: "h-10 w-10", text: "text-sm", status: "h-2.5 w-2.5 ring-2" },
  lg: { container: "h-12 w-12", text: "text-base", status: "h-3 w-3 ring-2" },
  xl: { container: "h-16 w-16", text: "text-lg", status: "h-3.5 w-3.5 ring-2" },
};

const statusColors: Record<string, string> = {
  online: "bg-green-500",
  offline: "bg-gray-400",
  busy: "bg-red-500",
  away: "bg-yellow-500",
};

function getInitials(name: string): string {
  return name
    .split(/\s+/)
    .map((w) => w[0])
    .filter(Boolean)
    .slice(0, 2)
    .join("")
    .toUpperCase();
}

function initialsColor(name: string): string {
  const colors = [
    "bg-blue-500", "bg-green-500", "bg-purple-500", "bg-pink-500",
    "bg-indigo-500", "bg-teal-500", "bg-orange-500", "bg-cyan-500",
  ];
  const hash = [...name].reduce((a, c) => a + c.charCodeAt(0), 0);
  return colors[hash % colors.length];
}

export default function Avatar({ src, alt, name, size = "md", status }: AvatarProps) {
  const s = sizeStyles[size];

  return (
    <div className={`relative inline-flex flex-shrink-0 ${s.container}`}>
      {src ? (
        <img
          src={src}
          alt={alt ?? name ?? "Avatar"}
          className={`${s.container} rounded-full object-cover`}
        />
      ) : name ? (
        <div
          className={`${s.container} flex items-center justify-center rounded-full text-white ${initialsColor(name)}`}
        >
          <span className={`font-medium leading-none ${s.text}`}>{getInitials(name)}</span>
        </div>
      ) : (
        <div className={`${s.container} flex items-center justify-center rounded-full bg-gray-200`}>
          <svg className="h-1/2 w-1/2 text-gray-500" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" d="M15.75 6a3.75 3.75 0 11-7.5 0 3.75 3.75 0 017.5 0zM4.501 20.118a7.5 7.5 0 0114.998 0A17.933 17.933 0 0112 21.75c-2.676 0-5.216-.584-7.499-1.632z" />
          </svg>
        </div>
      )}
      {status && (
        <span
          className={`absolute bottom-0 right-0 block rounded-full ring-white ${s.status} ${statusColors[status]}`}
        />
      )}
    </div>
  );
}
