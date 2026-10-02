# Agent GitHub Token

Short-lived GitHub tokens for AI coding agents, issued through your own GitHub App.

You authorize once in the browser. After that, agents get tokens that renew automatically, and the App decides which repositories they can reach. No personal access token, App private key or client secret is stored on your machine.

## How It Works

```
agent-github-token login            (once: approve the App in the browser)
        │
        ▼
macOS Keychain: access token (valid 8 hours) + refresh token (valid 6 months)
        │
        ▼
agent runs `agent-github-token gh ...` or `git push`
        │
        ├─ access token still valid? ── yes ──► use it
        │
        └─ expires within 10 minutes ──► renew with the refresh token,
                                          store the new pair, use it
```

*   **Login:** `agent-github-token login` runs GitHub's [device flow](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app#using-the-device-flow-to-generate-a-user-access-token). You open `https://github.com/login/device`, enter the code shown, and approve the App.
*   **Renewal:** access tokens last 8 hours and are renewed automatically. Each renewal also replaces the refresh token, so the 6-month limit restarts every time the tool is used. You only need to log in again if it goes unused for 6 months, or if you revoke the authorization.
*   **Access:** tokens only reach repositories the App is installed on, and only with permissions that both the App and your own account have. Actions show up as you, made through the App.

The tool has two parts, shipped together in `bin/`:

*   **`gh-app-token-generator`** (Go) runs the device flow, stores and renews the tokens, and acts as a git credential helper.
*   **`agent-github-token`** (shell) holds your App's Client ID and is the command you use.

## Prerequisites

*   **macOS:** the tokens are stored in the macOS Keychain.
*   **GitHub CLI (`gh`):** for `agent-github-token gh`. ([Installation Guide](https://github.com/cli/cli#installation))

## Setup

### Step 1: Create a GitHub App

1.  Go to GitHub **Settings** > **Developer settings** > **GitHub Apps** and click **New GitHub App**.
2.  Fill out the form:
    *   **GitHub App name:** for example, "My Coding Agent".
    *   **Homepage URL:** a placeholder is fine.
    *   **Enable Device Flow:** check it.
    *   **Expire user authorization tokens:** leave it checked (the default). The tool relies on expiring tokens.
    *   **Webhook:** uncheck **Active**.
    *   **Permissions:** grant only what the agent needs. For example, `Contents: Read and write` to push branches and `Pull requests: Read and write` to open pull requests.
    *   **Where can this GitHub App be installed?:** **Only on this account**.
3.  Click **Create GitHub App**, and note the **Client ID** on the next page. This is not the App ID.

You don't need a client secret or a private key.

### Step 2: Install the App on Repositories

1.  In the App's settings, click **Install App**, then **Install** next to your account.
2.  Choose **Only select repositories** and pick the repositories agents may work on.

To change the selection later, go to **Settings** > **Applications** > **Installed GitHub Apps** > **Configure**.

### Step 3: Download and Configure the Tool

1.  Download the macOS (darwin) archive from the [Releases page](https://github.com/oikarinen/agent-gh-token-generator/releases) and unpack it. The `agent-github-token` script and the `gh-app-token-generator` binary are both in `bin/`. Keep them together.
2.  Set your App's Client ID at the top of `bin/agent-github-token`:

    ```bash
    GH_APP_CLIENT_ID="${GH_APP_CLIENT_ID:-Iv23liYourClientID}"
    ```

    You can also set `GH_APP_CLIENT_ID` in your environment instead. Only `login` needs it.

If macOS refuses to run the binary because it is from an unidentified developer, remove the download quarantine flag:

```bash
xattr -d com.apple.quarantine bin/gh-app-token-generator
```

### Step 4: Log In

```bash
./bin/agent-github-token login
```

Open the URL shown, enter the code, and approve the App. Check the result with:

```bash
./bin/agent-github-token status
```

## Usage

**GitHub CLI:** run `gh` through the tool. It gets a fresh token for each command and leaves your own `gh` login unchanged:

```bash
agent-github-token gh pr create --fill
```

For Claude Code, see [Claude Code](#claude-code) below: it makes plain `gh` and `git` in Claude's sessions use the App token, without changing your own terminal.

**git:** in the repository the agent works in, run:

```bash
agent-github-token setup-git
```

git then gets a token from the tool for every `https://github.com` request in that repository. Credential helpers only apply to HTTPS remotes: a remote like `git@github.com:owner/repo.git` uses your own SSH key instead, and `setup-git` warns about it. Switch such remotes to HTTPS with `git remote set-url`.

**Other tools:** `agent-github-token token` prints a valid token. Use it for a single command, for example `GH_TOKEN="$(agent-github-token token)" some-tool`. Don't keep it around: each renewal invalidates the previous token.

**Logging out:** `agent-github-token logout` removes the tokens from the Keychain. To also revoke the App's access, go to [Settings > Applications > Authorized GitHub Apps](https://github.com/settings/apps/authorizations).

## Claude Code

You can limit Claude Code's GitHub access to this App's token, while your own terminal keeps your personal login.

1.  Log in as described above.
2.  Print the settings and merge them into your user settings, `~/.claude/settings.json`:

    ```bash
    agent-github-token claude-settings
    ```

    They have to go in your user settings: Claude Code ignores credential rules in a repository's settings files.
3.  Start a new Claude Code session.

What the settings do:

*   **Session start:** a hook puts a `gh` shim first on the `PATH` of Claude's commands. The shim gets a token for your App and runs the real `gh` with it, using a separate `gh` config directory, so `gh` can't fall back to your own login. The hook also points Claude's `git` at this tool for `https://github.com` and rewrites `git@github.com:` remotes to HTTPS. All of this is set through environment variables that only Claude's commands see.
*   **Before every command:** a second hook blocks commands that reach for other credentials: the Keychain through `security`, `gh auth`, SSH keys, `gh`'s config, other git credential helpers, setting `GH_TOKEN` by hand, or changing how git authenticates. Before commands that talk to GitHub, it renews the token if needed and has GitHub confirm the token belongs to your App's client ID (GitHub is asked once per token). Otherwise the command is blocked with an explanation, such as asking you to log in again.
*   **Sandbox:** every command runs in Claude Code's sandbox with no unsandboxed fallback. Credential files (`~/.ssh`, `~/.config/gh`, `~/.git-credentials`, `~/.netrc`) and token variables are hidden from it, and the Read tool is denied those files. Network access is limited to GitHub, so add the other domains your work needs. `enableWeakerNetworkIsolation` lets `gh`, which is written in Go, verify TLS certificates inside the sandbox; Claude Code's documentation notes that this opens a potential data exfiltration path through the macOS trust service.

What it can't guarantee:

*   **The Keychain stays reachable.** Sandboxed commands can still query the macOS Keychain. The hook blocks the direct ways in, but a roundabout command, such as a script, could get there. Any other GitHub credential in your login Keychain is therefore within reach, including `gh`'s own login (`gh auth login` stores its token there) and passwords saved by git's osxkeychain helper. For a firm guarantee, keep no other GitHub credentials in the Keychain: log `gh` in with `gh auth login --insecure-storage` so its token lives in `~/.config/gh`, which the sandbox hides, or log it out, and remove `github.com` entries in Keychain Access. The strongest option is to run Claude Code under a separate macOS user.
*   **Hooks match command text.** They are guardrails against mistakes and casual misuse, not a security boundary. The sandbox is the boundary.
*   **The App's own tokens are reachable too.** Claude's commands need them, and they only grant the same restricted access.

## Security Notes

*   Tokens are passed to the `security` tool on stdin, never on a command line.
*   The Keychain item is protected from other users and stays out of plain-text files, but any program running as your user can read it with the `security` tool. Treat the machine account the agent runs under accordingly.
*   Renewal invalidates the previous token pair, so processes take a lock file (in `~/Library/Caches/agent-github-token/`) to make sure only one of them renews at a time. If the lock file can't be created, for example inside a sandbox, the tool carries on without it and picks up tokens another process renewed in the meantime.

## Coming From the Private-Key Setup

Earlier development versions authenticated as the App's installation, using the App's private key stored in the Keychain. That key is no longer used. Remove it from the Keychain:

```bash
security delete-generic-password -s agent-github-token -a GH_APP_PRIVATE_KEY
```

Then delete the private key in the App's settings, unless something else uses it.
