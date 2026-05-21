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

## Automation preference

Prefer these clients in order:

1. Bun native `WebView.cdp`
2. `pydoll-python`
3. Playwright

Close only pages, views, or contexts you created. Do not close the shared browser unless the user explicitly asks.

## Option 1: Bun native WebView.cdp

Use Bun's native WebView when available. Connect it to the Cloakbrowser CDP endpoint, navigate at least once to initialize the view session, then send raw CDP commands through `view.cdp(...)`.

```javascript
const base = process.env.POD_WEB_BROWSER || process.env.WEB_BROWSER;
if (!base) throw new Error("Missing POD_WEB_BROWSER");

function cdpWebSocketUrl(baseUrl, cdpUrl) {
  const url = new URL(cdpUrl, baseUrl);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  return url.toString();
}

const profiles = await fetch(`${base}/api/profiles`).then((r) => r.json());
const profile =
  profiles.find((p) => p.name === "NUC" && p.status === "running") ||
  profiles.find((p) => p.status === "running");
if (!profile) throw new Error("No running Cloakbrowser profile");

const view = new Bun.WebView({
  backend: { type: "chrome", url: cdpWebSocketUrl(base, profile.cdp_url) },
});
await view.navigate("https://example.com");

const { root } = await view.cdp("DOM.getDocument");
const { nodeId } = await view.cdp("DOM.querySelector", {
  nodeId: root.nodeId,
  selector: "input#search",
});
await view.cdp("DOM.focus", { nodeId });
```

`WebView.cdp<T>(method, params?)` sends one Chrome DevTools Protocol command scoped to the current tab. Method names are domain-qualified strings such as `Runtime.evaluate`, `DOM.querySelector`, or `Emulation.setUserAgentOverride`; params must be JSON-serializable.

## Option 2: pydoll-python

Use `pydoll-python` when Python is the better fit or Bun WebView is unavailable. Connect to the Cloakbrowser CDP endpoint discovered from `$POD_WEB_BROWSER`.

```python
import asyncio
import os
from urllib.parse import urljoin, urlparse, urlunparse

import httpx
from pydoll.browser.chromium import Chrome


def cdp_websocket_url(base_url, cdp_url):
    parsed = urlparse(urljoin(base_url, cdp_url))
    scheme = "wss" if parsed.scheme == "https" else "ws"
    return urlunparse(parsed._replace(scheme=scheme))


async def main():
    base = os.environ.get("POD_WEB_BROWSER") or os.environ.get("WEB_BROWSER")
    if not base:
        raise RuntimeError("Missing POD_WEB_BROWSER")

    profiles = httpx.get(f"{base}/api/profiles", timeout=10).json()
    profile = next(
        (p for p in profiles if p.get("name") == "NUC" and p.get("status") == "running"),
        None,
    ) or next((p for p in profiles if p.get("status") == "running"), None)
    if not profile:
        raise RuntimeError("No running Cloakbrowser profile")

    browser = Chrome()
    tab = await browser.connect(cdp_websocket_url(base, profile["cdp_url"]))
    await tab.go_to("https://example.com")
    title = await tab.execute_script("return document.title")
    print(title)
    await browser.close()


asyncio.run(main())
```

If the installed `pydoll-python` version exposes a different connection helper, use its CDP/WebSocket connection API with the same discovered endpoint.

## Option 3: Playwright

Use Playwright as the fallback CDP client:

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
