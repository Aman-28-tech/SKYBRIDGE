import type { Metadata } from "next";
import type { ReactNode } from "react";
import ThemeToggle from "@/components/ThemeToggle";
import "./globals.css";

export const dynamic = "force-dynamic";

export const metadata: Metadata = {
  title: "SKYBRIDGE Console",
  description: "Read-only SKYBRIDGE migration control-plane console (local demo).",
};

const LINKS = [
  { href: "/", label: "Dashboard" },
  { href: "/migrations", label: "Migration" },
  { href: "/ownership", label: "Ownership" },
  { href: "/cdc", label: "CDC" },
  { href: "/cutover", label: "Cutover" },
  { href: "/safety", label: "Safety" },
  { href: "/evidence", label: "Evidence" },
  { href: "/demo", label: "Live Demo" },
];

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>
        <a href="#main" className="skip-link" style={{ position: "absolute", left: "-9999px" }}>
          Skip to content
        </a>
        <header className="topbar">
          <div className="brand">
            <span className="brand-mark" aria-hidden="true">S</span>
            <h1>SKYBRIDGE Console</h1>
            <span className="readonly-pill">Read-only · v1</span>
          </div>
          <div role="group" aria-label="Theme">
            <ThemeToggle />
          </div>
        </header>
        <nav className="nav" aria-label="Console sections">
          {LINKS.map((l) => (
            <a key={l.href} href={l.href} data-testid={`nav-${l.label}`}>
              {l.label}
            </a>
          ))}
        </nav>
        <main id="main" className="main">
          {children}
        </main>
        <footer className="footer">
          SKYBRIDGE Console v1 · read-only observability layer · no cloud mutation · local demo only
        </footer>
      </body>
    </html>
  );
}
