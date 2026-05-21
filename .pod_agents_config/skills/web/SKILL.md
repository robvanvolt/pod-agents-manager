---
name: web
description: Use when the user asks to open, inspect, browse, automate, debug, or screenshot a website through the configured Cloakbrowser/CDP browser service. Uses POD_WEB_BROWSER from the pod environment.
---

# Web Browser Skill

Use the shared browser service from `$POD_WEB_BROWSER` (or `$WEB_BROWSER`) instead of launching a local browser.

## Required env

```bash
BASE="${POD_WEB_BROWSER:-${WEB_BROWSER:-}}"
[ -n "$BASE" ] || { echo "Missing POD_WEB_BROWSER in ~/.pod_agents_config/.env"; exit 1; }
```

`$BASE` is the Cloakbrowser HTTP API root, for example `http://192.168.178.142:8686`.

## Discover the running profile

```bash
curl -fsS "$BASE/api/profiles"
```

Pick a profile with `"status":"running"`. Prefer the profile named `NUC` when present.

Each profile has a `cdp_url`, usually like:

```text
/api/profiles/<profile-id>/cdp
```

The CDP endpoint is:

```bash
CDP="$BASE$cdp_url"
```

## Connect over CDP

Use a CDP-capable client, for example Playwright:

```javascript
const { chromium } = require("playwright");

const base = process.env.POD_WEB_BROWSER || process.env.WEB_BROWSER;
if (!base) throw new Error("Missing POD_WEB_BROWSER");

const profiles = await fetch(`${base}/api/profiles`).then((r) => r.json());
const profile =
  profiles.find((p) => p.name === "NUC" && p.status === "running") ||
  profiles.find((p) => p.status === "running");
if (!profile) throw new Error("No running Cloakbrowser profile");

const browser = await chromium.connectOverCDP(`${base}${profile.cdp_url}`);
const context = browser.contexts()[0] || await browser.newContext();
const page = context.pages()[0] || await context.newPage();
await page.goto("https://example.com");
```

Close only pages or contexts you created. Do not close the shared browser unless the user explicitly asks.
