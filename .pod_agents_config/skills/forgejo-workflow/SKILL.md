---
name: forgejo-workflow
description: Use when asked to create a new repository on the user's self-hosted Forgejo server, initialize a local git environment, and push the code. Handles both first-time setup and re-running on an existing local repo.
---

When the user asks to create a new repository and push code to Forgejo, follow these exact steps.

## Prerequisites

The pod already has these environment variables set from `~/.pod_agents_config/.env`:

| Variable          | Used for                                            |
| ----------------- | --------------------------------------------------- |
| `$FORGEJO_URL`    | Base URL of the Forgejo instance, e.g. `https://git.example.com` |
| `$FORGEJO_USER`   | Username that owns the new repo                     |
| `$FORGEJO_EMAIL`  | Email used as `git commit` author                   |
| `$FORGEJO_TOKEN`  | API token (also used for HTTPS push auth)           |

**Do not ask the user for their password or token.** Do not prompt for any of these values. If any of them is empty, stop and tell the user to populate the matching `POD_FORGEJO_*` line in `.env`.

Sanity check before running anything:

```bash
[ -n "$FORGEJO_URL" ] && [ -n "$FORGEJO_USER" ] && [ -n "$FORGEJO_EMAIL" ] && [ -n "$FORGEJO_TOKEN" ] \
  || { echo "Missing one of POD_FORGEJO_{URL,USER,EMAIL,TOKEN} in ~/.pod_agents_config/.env"; exit 1; }
```

## Step 1: Identify the repo name

If the user didn't specify a repo name, use the current directory's name:

```bash
REPO_NAME="${REPO_NAME:-$(basename "$PWD")}"
```

## Step 2: Create the remote repository via API

Use the token in the `Authorization` header. Treat HTTP 201 (created) and HTTP 409 (already exists) as success — the skill should be idempotent on re-run.

```bash
status=$(curl -sS -o /tmp/forgejo_create.json -w '%{http_code}' \
  -X POST "$FORGEJO_URL/api/v1/user/repos" \
  -H "Authorization: token $FORGEJO_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"$REPO_NAME\",\"private\":true,\"default_branch\":\"main\",\"auto_init\":false}")

case "$status" in
  201) echo "Created: $FORGEJO_URL/$FORGEJO_USER/$REPO_NAME" ;;
  409) echo "Repo already exists — continuing" ;;
  *)   echo "Create failed (HTTP $status):"; cat /tmp/forgejo_create.json; exit 1 ;;
esac
```

## Step 3: Initialize the local git repo (idempotent)

Skip `git init` if the directory is already a repo. Use `main` as the default branch so it matches what we created on the server. Set the author identity from `$FORGEJO_USER` / `$FORGEJO_EMAIL` — without this, `git commit` either fails with "Please tell me who you are" or attributes commits to `root@<container-id>`.

```bash
if [ ! -d .git ]; then
  git init -b main
fi
git config user.name  "$FORGEJO_USER"
git config user.email "$FORGEJO_EMAIL"

# Stage and commit only if there's actually something staged/changed.
git add -A
if ! git diff --cached --quiet; then
  git commit -m "Initial commit"
else
  echo "Nothing new to commit"
fi
```

## Step 4: Add the remote with token-authenticated HTTPS

Use HTTPS rather than SSH — pods don't have SSH keys configured for git push. Embed the token in the URL so `git push` authenticates without prompting. Forgejo accepts the token as the password with any username; `oauth2` is the conventional placeholder.

```bash
# Domain from $FORGEJO_URL, e.g. https://git.example.com → git.example.com
DOMAIN=$(echo "$FORGEJO_URL" | awk -F/ '{print $3}')
SCHEME=$(echo "$FORGEJO_URL" | awk -F: '{print $1}')

REMOTE_URL="$SCHEME://oauth2:$FORGEJO_TOKEN@$DOMAIN/$FORGEJO_USER/$REPO_NAME.git"

if git remote get-url origin >/dev/null 2>&1; then
  git remote set-url origin "$REMOTE_URL"
else
  git remote add origin "$REMOTE_URL"
fi
```

## Step 5: Push

```bash
git push -u origin main
```

If the push succeeds, the repo is live at `$FORGEJO_URL/$FORGEJO_USER/$REPO_NAME`.

## Security notes

- The token ends up in `.git/config` because it's part of the remote URL. That's acceptable inside a pod sandbox (per-instance workspace, ephemeral, only the user can read it), but **do not** copy this workspace to a shared location without first stripping the token:

  ```bash
  git remote set-url origin "$SCHEME://$DOMAIN/$FORGEJO_USER/$REPO_NAME.git"
  ```

  Subsequent pushes will then prompt for credentials.
- The token is also exported into the container env as `$FORGEJO_TOKEN`, so an alternative for token-out-of-disk hygiene is to keep the remote URL token-free and pass auth per-push via:

  ```bash
  git -c http.extraHeader="Authorization: token $FORGEJO_TOKEN" push -u origin main
  ```

## Troubleshooting

- **`Authentication failed` on push** — the token is missing the `repo` scope. Recreate it at `$FORGEJO_URL/user/settings/applications` with `repo` checked.
- **`fatal: unable to access … SSL certificate problem`** — your Forgejo uses a self-signed cert. Either install the CA into the container or set `GIT_SSL_NO_VERIFY=true` for the push (don't make it permanent).
- **HTTP 403 on `POST /api/v1/user/repos`** — token lacks user/repo write scope, or `$FORGEJO_USER` doesn't match the token's owner. The token must belong to the user that will own the repo.
- **Want to push to an organization instead of your user account?** Replace Step 2's endpoint with `POST /api/v1/orgs/<org>/repos` and use `$ORG` in place of `$FORGEJO_USER` in Step 4's URL.
