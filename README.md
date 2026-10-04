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
| 2 | user | Opens the link the agent shows and logs in with their Google account. |
| 2b | user, new Cloud accounts only | Opens the Google Cloud console once and accepts the terms. |
| 3 | agent | Creates a project for the user, enables the Drive and Sheets APIs, creates a robot (service) account and its key. |
| 4 | agent | Shares the user's chosen folder with the robot. |
| 5 | agent | Works through the API from then on: read files, edit sheet cells in place, create files, watch for changes. |

Written in Go: one binary per platform, official Google client libraries.

## Known Google limits

- **New Cloud accounts must accept the Cloud terms once in the browser.** Creating a
  project from the CLI fails until they do.
- **Robot accounts cannot create files in a personal Drive.** Service accounts created
  after 15 April 2025 cannot own Drive items, even in a folder shared with them
  ([details](https://forum.rclone.org/t/google-drive-service-account-changes-and-rclone/50136)).
  The fix drivekey plans to use: the login in step 2 also grants Drive access
  (`gcloud auth login --enable-gdrive-access`), so new files are created as the user
  and owned by them.
- That login is powerful: it covers the user's Google Cloud and their whole Drive.
  drivekey has to store it carefully and say so plainly.

## Open questions being tested

1. Does a personal Gmail account get through the Drive-enabled `gcloud` login without
   an "app blocked" screen?
2. Can that login create files and edit Sheets cells directly?
3. Are those API calls counted against the user's own project (own rate limits),
   not against `gcloud`'s shared one?
4. Can the robot account edit cells and replace file contents in a shared folder,
   and does it see files added later?

## License

To be decided.
