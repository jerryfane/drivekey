# drivekey

Give your AI agent its own access to your Google Drive and Google Sheets.
You open one link, log in with your Google account, and you are done.
No clicking through the Google Cloud console, no app to register, nothing pre-configured.

> **Status: early.** All commands work against a real personal Google account; no
> release has been tagged yet. Build from source: `go build ./cmd/drivekey`.

## Why

Tools that talk to Google Drive from a terminal usually borrow a shared login app
(rclone's built-in client, for example). Those shared apps are slow, rate-limited,
and going away: Google will start charging for heavy API use on them later in 2026,
and rclone is retiring its built-in client because of it
([rclone forum](https://forum.rclone.org/t/google-drive-and-google-photos-users-action-required/54005),
[Google notice](https://developers.google.com/workspace/tools-safety)).

The alternative today is "make your own client ID", which means the Google Cloud
console, a consent screen, a homepage URL and a privacy policy. Most people stop there.

drivekey gives each user their **own** Google project, created for them by their agent,
so they get their own rate limits and nothing depends on a shared app.

## Quick start

Agents: follow [AGENTS.md](AGENTS.md). It is the exact sequence, including error codes.

```sh
curl -fsSL https://raw.githubusercontent.com/jerryfane/drivekey/main/install.sh | sh
drivekey login                 # prints a URL; the user opens it, logs in, copies the code
drivekey login --code CODE
drivekey setup                 # creates the user's own Google Cloud project, enables Drive + Sheets
drivekey ls                    # done
```

| Step | Who | What |
|---|---|---|
| 1 | agent | Installs drivekey and Google's `gcloud` CLI. |
| 2 | user | Opens the link the agent shows, logs in with their Google account, pastes back the code. |
| 2b | user, new Cloud accounts only | Opens https://console.cloud.google.com once and accepts the terms. |
| 3 | agent | `drivekey setup` creates a project for the user and enables the Drive and Sheets APIs. |
| 4 | agent | Works as the user from then on. |

Everything the agent creates is owned by the user, in their own Drive.

## Commands

| Command | What it does |
|---|---|
| `ls`, `info`, `get`, `put`, `mkdir` | List, inspect, download (with `--export` for Google Docs files), upload or replace contents, create folders. |
| `sheet create`, `sheet tabs`, `sheet read`, `sheet write`, `sheet append` | Work with Google Sheets cell by cell, never by re-uploading the whole file. |
| `sheet set` | Set one cell, finding the row by a key column and the column by its header name. Refuses to guess. |
| `changes`, `watch` | What changed since last time, optionally limited to one folder and everything inside it. |
| `status`, `login`, `setup`, `logout` | Account management. |

Every command prints JSON. Errors go to stderr as `{"error": {"code", "message", "hint"}}`.

## How it works

- **Login** is Google's own `gcloud auth login --enable-gdrive-access`, so drivekey never
  registers or ships an OAuth client. drivekey keeps a private gcloud config
  (`~/.config/drivekey/gcloud`, mode 0700) and never touches the user's own gcloud setup.
  The login runs in a small background helper, so an agent can print the URL in one step
  and hand over the code in the next.
- **Own project:** every API call sends the user's project as quota project
  (`x-goog-user-project`), so the user gets their own rate limits.
- **Tokens:** drivekey asks gcloud for a short-lived access token and caches it until it
  expires. It never reads gcloud's refresh token.
- Written in Go with Google's official client libraries; one binary per platform.

## Known Google limits

- **New Cloud accounts must accept the Cloud terms once in the browser.** Creating a
  project from the CLI fails until they do.
- **No robot (service) account.** Service accounts created after 15 April 2025 cannot
  own Drive items, so they cannot create files in a personal Drive
  ([details](https://forum.rclone.org/t/google-drive-service-account-changes-and-rclone/50136)).
  drivekey uses the user's own login for everything instead.
- **The login is powerful:** it covers the user's Google Cloud and their whole Drive.
  It is stored only in drivekey's private directory (mode 0700 on Linux and macOS; on
  Windows it relies on the user profile's default permissions, so keep `DRIVEKEY_HOME`
  inside your profile). Revoke it with `drivekey logout`, or at
  https://myaccount.google.com/permissions ("Google Cloud SDK").
- **gcloud is required** for the login step (about 500 MB). It is the only way to log in
  without registering an app.

## Test results (2026-10-04, fresh personal Gmail account)

| Question | Result |
|---|---|
| Drive-enabled `gcloud` login from a link, no "app blocked" screen? | Yes. |
| Project creation before accepting Cloud terms? | Fails: `Callers must accept Terms of Service`. One console visit needed. |
| Robot (service account) key creation on a personal account? | Works; no org policy blocks it. (Robot later dropped from the design.) |
| User login creates folders, Sheets, uploads? | Yes, owned by the user. |
| Robot edits Sheet cells / replaces file contents / sees later files? | Yes. |
| Robot creates files (upload or empty Sheet) in the shared folder? | No: `storageQuotaExceeded`. It can create subfolders. |
| User login edits Sheet cells, replaces file contents, sees changes? | Yes. |
| User calls counted against the user's own project? | Only when `x-goog-user-project` is set. Without it they count against `gcloud`'s shared project, which already returned 429 rate limits during the test. |

Design consequences:

- No robot: the user's login does everything, including creating files.
- Every API call names the user's project as quota project.
- `gcloud` is only used for login and project setup.

## License

To be decided.
