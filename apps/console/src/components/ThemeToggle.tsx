"use client";

export default function ThemeToggle() {
  return (
    <button
      type="button"
      className="btn"
      data-testid="theme-toggle"
      aria-label="Toggle color theme"
      onClick={() => {
        const el = document.documentElement;
        const cur = el.getAttribute("data-theme");
        el.setAttribute("data-theme", cur === "light" ? "dark" : "light");
      }}
    >
      Toggle theme
    </button>
  );
}
