# drivekey

Give your AI agent its own access to your Google Drive and Google Sheets.
You open one link, log in with your Google account, and you are done.
No clicking through the Google Cloud console, no app to register, nothing pre-configured.

> **Status: design.** Nothing is implemented yet. This README is the plan; the
> open questions below are being tested before any code is written.

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

## How it works (planned)

| Step | Who | What |
|---|---|---|
| 1 | agent | Installs Google's `gcloud` CLI if it is missing. |
| 2 | user | Opens the link the agent shows and logs in with their Google account (`gcloud auth login --enable-gdrive-access`). |
| 2b | user, new Cloud accounts only | Opens the Google Cloud console once and accepts the terms. |
| 3 | agent | Creates a project for the user and enables the Drive and Sheets APIs. |
| 4 | agent | Works as the user from then on: create files and folders, read files, edit sheet cells in place, replace file contents, watch for changes. Every call names the user's project (`x-goog-user-project`) so it uses the user's own rate limits. |

Everything the agent creates is owned by the user, in their own Drive.

Written in Go: one binary per platform, official Google client libraries.

## Known Google limits

- **New Cloud accounts must accept the Cloud terms once in the browser.** Creating a
  project from the CLI fails until they do.
- **No robot (service) account.** Service accounts created after 15 April 2025 cannot
  own Drive items, so they cannot create files in a personal Drive
  ([details](https://forum.rclone.org/t/google-drive-service-account-changes-and-rclone/50136)).
  drivekey uses the user's own login for everything instead.
- **The login is powerful:** it covers the user's Google Cloud and their whole Drive.
  drivekey has to store it carefully and say so plainly.

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
