# Using drivekey from an agent

drivekey lets you, an AI agent, read and change your user's own Google Drive and Google Sheets.
Every command prints JSON on stdout. On failure it prints `{"error": {"code", "message", "hint"}}`
on stderr and exits non-zero. Branch on `error.code`, and show `hint` to the user when it asks
them to do something.

## 1. Install

```sh
curl -fsSL https://raw.githubusercontent.com/jerryfane/drivekey/main/install.sh | sh
```

drivekey needs Google's `gcloud` CLI for the login step only. If `drivekey status` reports
`gcloud_missing`, install it (`curl https://sdk.cloud.google.com | bash`, or see
https://cloud.google.com/sdk/docs/install), or ask the user to.

## 2. Log in (needs the user once)

```sh
drivekey login
```

The output has a `url` field. Send that URL to the user and tell them:

> Open this link, log in with your Google account, click Allow, and send me the code Google shows.

When they send the code:

```sh
drivekey login --code 'THE_CODE'
```

- The code expires within minutes. If you get `login_failed` or `no_login_pending`, run
  `drivekey login` again and send the new URL.
- Google warns that this grants access to their Google Cloud and all of their Drive. That is
  expected: drivekey needs it to create their own project and to create files they own.

## 3. Set up (once)

```sh
drivekey setup
```

This creates the user's own Google Cloud project and enables the Drive and Sheets APIs, so
they get their own rate limits.

- `cloud_terms_not_accepted`: the user has never used Google Cloud. Ask them to open
  https://console.cloud.google.com while logged in to the same account and accept the terms.
  Then run `drivekey setup` again. It reuses the same project id.
- `rate_limited`: wait a minute and run it again.

`drivekey status` shows where you are; its `next` field names the next command to run.

## 4. Work

| Task | Command |
|---|---|
| List a folder | `drivekey ls FOLDER_ID` (no id = whole Drive; `--query "name contains 'cv'"`) |
| File details | `drivekey info FILE_ID` |
| Download a file | `drivekey get FILE_ID --out DIR_OR_PATH` |
| Download a Google Doc/Sheet/Slides | `drivekey get FILE_ID --export pdf` (or `xlsx`, `csv`, `docx`, ...) |
| Upload a new file | `drivekey put ./file.pdf --parent FOLDER_ID` |
| Replace a file's contents (same id and link) | `drivekey put ./file.pdf --replace FILE_ID` |
| Create a folder | `drivekey mkdir NAME --parent FOLDER_ID` |
| Create a Google Sheet | `drivekey sheet create NAME --parent FOLDER_ID` |
| Create a Google Sheet/Doc/Slides from a file | `drivekey put ./Roadmap.xlsx --parent FOLDER_ID --convert` (xlsx, csv, docx, pptx, ...) |
| List tabs | `drivekey sheet tabs SHEET_ID` |
| Read cells | `drivekey sheet read SHEET_ID "'Tab'!A1:F50"` |
| Write a range | `drivekey sheet write SHEET_ID "'Tab'!B2:C3" --values '[["a","b"],["c","d"]]'` |
| Add rows | `drivekey sheet append SHEET_ID Tab --values '[["x","y"]]'` |
| Set one cell by row key and column name | `drivekey sheet set SHEET_ID --tab Tab --key-col id --key 42 --col status --value done` |
| What changed since last time | `drivekey changes --folder FOLDER_ID` |
| Stream changes | `drivekey watch --folder FOLDER_ID --interval 60s` |

Rules that keep the user's data safe:

- **Never replace a whole spreadsheet to change some cells.** Use `sheet write` or `sheet set`;
  they only touch the cells you name, so other people's edits survive. `put --replace` refuses
  to overwrite Google Docs files for this reason.
- **Prefer `sheet set` for single cells.** It finds the row by key and the column by header
  name. If they are missing or ambiguous it fails (`row_not_found`, `column_not_found`,
  `ambiguous_key`) rather than guessing. On `row_moved`, someone is editing: run it again.
- Values are parsed like typed input (`=SUM(...)`, numbers, dates) unless you pass `--raw`.
- The first `drivekey changes` call only starts tracking and returns nothing. Changes are
  reported at least once: if two `changes`/`watch` runs read the same feed at the same time,
  both may report a change, so treat a change as "look at this file again", not as a counter.
- Folder and file ids are the long strings in Drive URLs: `drive.google.com/drive/folders/<id>`,
  `docs.google.com/spreadsheets/d/<id>/edit`.

## Errors

| code | meaning | do |
|---|---|---|
| `gcloud_missing` | gcloud not installed | install it (step 1) |
| `not_logged_in` | no login, or it expired | step 2 |
| `not_set_up` | setup not finished | step 3 |
| `cloud_terms_not_accepted` | Cloud terms not accepted | user accepts at console.cloud.google.com, then `drivekey setup` |
| `api_disabled` | APIs still enabling | wait a minute; else `drivekey setup` |
| `not_found` | wrong id, or no access | check the id |
| `permission_denied` | the user cannot edit that file | tell the user |
| `rate_limited` | Google rate limit | wait and retry |
| `export_required` | Google Docs file needs `--export` | pick a format from `hint` |
| `row_not_found`, `column_not_found`, `ambiguous_key`, `ambiguous_column`, `row_moved` | `sheet set` could not safely find one cell | read the sheet and fix the arguments |

## Revoking access

`drivekey logout` revokes the login and deletes local state. The user can also remove access
at https://myaccount.google.com/permissions ("Google Cloud SDK"). The Cloud project is kept;
delete it in the Cloud console if it is no longer needed.
