"use client";

import { type CtaId, captureCta } from "@codeswhat/public-analytics";
import { PackageOpen, ShieldCheck, Terminal, TriangleAlert, Zap } from "lucide-react";
import { usePathname } from "next/navigation";
import { useState } from "react";
import { DockerRunSnippet } from "@/components/docker-run-snippet";
import { SectionHeading } from "@/components/section-heading";
import { TrackedLink } from "@/components/tracked-link";
import { YamlBlock } from "@/components/yaml-block";
import { SITE_CONFIG } from "@/lib/site-config";

type Tab = "quick" | "secure" | "native";

const TABS: { id: Tab; label: string; icon: typeof Zap }[] = [
  { id: "quick", label: "Quick", icon: Zap },
  { id: "secure", label: "Secure", icon: ShieldCheck },
  { id: "native", label: "Native", icon: PackageOpen },
];

const INSTALL_CTA: Record<Tab, CtaId> = {
  quick: "install_quick",
  secure: "install_secure",
  native: "install_native",
};

// Hardened standard-mode compose from docs/getting-started.mdx.
// Runs read_only, drops all caps, and delivers the token as a
// Docker secret instead of an inline environment variable.
const dockerCompose = `# Portwing — standard mode, hardened defaults.
# The image runs as the non-root portwing user (UID 65532). Generate a token
# it can read, and export the numeric group ID of your Docker socket:
#   openssl rand -hex 32 > portwing_token.txt
#   sudo chown 65532:65532 portwing_token.txt && sudo chmod 0400 portwing_token.txt
#   export DOCKER_SOCK_GID=$(stat -c '%g' /var/run/docker.sock)
#
# This plaintext example publishes only on host loopback. For remote access,
# configure Portwing TLS before changing this bind, or keep the plaintext
# listener private behind a TLS-terminating reverse proxy.

services:
  portwing:
    image: ${SITE_CONFIG.dockerImage}:latest
    restart: unless-stopped
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - /tmp
    user: "65532:65532"  # image default; explicit so it survives image overrides
    group_add:
      - "\${DOCKER_SOCK_GID:?set to the GID of /var/run/docker.sock (see header)}"
    ports:
      - "127.0.0.1:3000:3000"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - portwing-stacks:/data/stacks
    environment:
      - TOKEN_FILE=/run/secrets/portwing_token
    secrets:
      - portwing_token

secrets:
  portwing_token:
    file: ./portwing_token.txt

volumes:
  portwing-stacks:`;

export function GetStarted() {
  const [tab, setTab] = useState<Tab>("quick");
  const pathname = usePathname();

  const selectTab = (nextTab: Tab) => {
    setTab(nextTab);
    captureCta(pathname, INSTALL_CTA[nextTab], "get_started");
  };

  return (
    <section className="border-t border-border/60 px-4 py-16">
      <div className="mx-auto max-w-3xl">
        <SectionHeading
          eyebrow="Get running"
          title="Get started in minutes"
          subtitle="Add Portwing to your compose file and point Drydock at it — one agent per host."
          align="right"
        />

        {/* Quick / Secure tab toggle */}
        <div className="mb-5 flex justify-center">
          <div
            role="tablist"
            aria-label="Install method"
            className="inline-flex gap-1 rounded-xl border border-neutral-200 bg-white/60 p-1 backdrop-blur-sm dark:border-neutral-800 dark:bg-neutral-900/60"
            onKeyDown={(e) => {
              const currentIndex = TABS.findIndex((t) => t.id === tab);
              if (e.key === "ArrowRight") {
                e.preventDefault();
                selectTab(TABS[(currentIndex + 1) % TABS.length].id);
              } else if (e.key === "ArrowLeft") {
                e.preventDefault();
                selectTab(TABS[(currentIndex - 1 + TABS.length) % TABS.length].id);
              } else if (e.key === "Home") {
                e.preventDefault();
                selectTab(TABS[0].id);
              } else if (e.key === "End") {
                e.preventDefault();
                selectTab(TABS[TABS.length - 1].id);
              }
            }}
          >
            {TABS.map(({ id, label, icon: Icon }) => {
              const active = tab === id;
              return (
                <button
                  key={id}
                  id={`tab-${id}`}
                  type="button"
                  role="tab"
                  aria-selected={active}
                  aria-controls="get-started-panel"
                  tabIndex={active ? 0 : -1}
                  onClick={() => selectTab(id)}
                  className={[
                    "flex items-center gap-1.5 rounded-lg px-4 py-1.5 text-sm font-medium transition-colors",
                    active
                      ? "bg-neutral-900 text-white dark:bg-neutral-100 dark:text-neutral-900"
                      : "text-neutral-500 hover:text-neutral-900 dark:text-neutral-400 dark:hover:text-neutral-100",
                  ].join(" ")}
                >
                  <Icon className="h-3.5 w-3.5" />
                  {label}
                </button>
              );
            })}
          </div>
        </div>

        <div role="tabpanel" id="get-started-panel" aria-labelledby={`tab-${tab}`}>
          {tab === "quick" ? (
            <DockerRunSnippet />
          ) : tab === "secure" ? (
            <div className="overflow-hidden rounded-xl border border-neutral-800 bg-neutral-950 shadow-2xl">
              <div className="flex items-center gap-2 border-b border-neutral-800 px-4 py-3">
                <Terminal className="h-4 w-4 text-neutral-500" />
                <span className="text-xs font-medium text-neutral-500">
                  docker-compose.standard.yml
                </span>
              </div>
              <YamlBlock
                code={dockerCompose}
                className="overflow-x-auto p-6 font-[family-name:var(--font-mono)] text-sm leading-relaxed text-neutral-300"
              />
            </div>
          ) : (
            <div className="overflow-hidden rounded-xl border border-neutral-800 bg-neutral-950 shadow-2xl">
              <div className="flex items-center gap-2 border-b border-neutral-800 px-4 py-3">
                <PackageOpen className="h-4 w-4 text-neutral-500" />
                <span className="text-xs font-medium text-neutral-500">Native packages</span>
              </div>
              <pre className="overflow-x-auto p-6 font-[family-name:var(--font-mono)] text-sm leading-relaxed text-neutral-300">
                <code>{`# macOS
brew install --cask codeswhat/tap/portwing

# Debian / Ubuntu
sudo apt install ./portwing_0.9.14_linux_amd64.deb

# Fedora / RHEL
sudo rpm --install ./portwing_0.9.14_linux_amd64.rpm`}</code>
              </pre>
            </div>
          )}

          <div className="mt-4 flex items-center justify-center gap-2 text-center text-sm">
            {tab === "quick" ? (
              <p className="flex items-center gap-2 text-neutral-500 dark:text-neutral-400">
                <TriangleAlert className="h-4 w-4 shrink-0 text-amber-500" />
                TOKEN is visible in docker inspect — fine for a local try, not for production.
              </p>
            ) : tab === "secure" ? (
              <p className="flex items-center gap-2 text-neutral-500 dark:text-neutral-400">
                <ShieldCheck className="h-4 w-4 shrink-0 text-violet-500" />
                Token delivered as a Docker secret — never in env vars or inspect output.{" "}
                <TrackedLink
                  href="/docs/getting-started"
                  ctaId="docs_getting_started"
                  placement="get_started"
                  className="font-medium text-neutral-900 underline-offset-4 hover:underline dark:text-neutral-100"
                >
                  Full getting-started docs →
                </TrackedLink>
              </p>
            ) : (
              <p className="flex items-center gap-2 text-neutral-500 dark:text-neutral-400">
                <ShieldCheck className="h-4 w-4 shrink-0 text-violet-500" />
                Stable packages are checksummed, keyless-signed, and install-tested.{" "}
                <TrackedLink
                  href="/docs/installation"
                  ctaId="docs_installation"
                  placement="get_started"
                  className="font-medium text-neutral-900 underline-offset-4 hover:underline dark:text-neutral-100"
                >
                  Install and verify →
                </TrackedLink>
              </p>
            )}
          </div>
        </div>
      </div>
    </section>
  );
}
