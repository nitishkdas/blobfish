# Writing for the BlobFish UI

How the web UI, the upload page and the API's error messages are worded.
Follow it for every new string, so the product reads as one voice.

## Voice

Plain, calm and specific. Say what happened and what to do next. No
exclamation marks, no jokes, no blame ("you entered an invalid…").

## Words

One name per idea, everywhere: UI, docs and API messages.

| Idea | Use | Not |
| --- | --- | --- |
| The name a person signs in with | username | user name, name |
| Signing in | sign in (verb), sign-in (noun) | log in, login |
| A file's path in a bucket | path (UI); key (API docs) | Key (in the UI) |
| Permanent address of a file in a public bucket | public link | public URL, public address |
| Revocable link someone made | share link | link (when it could be either) |
| What a key or user may do | access (UI); scopes (API) | permissions |
| `*:admin` | admin of all buckets (server admin) | All buckets + Admin |
| Removed with no way back | permanently deleted | purged, deleted for good, deleted forever, gone for good |
| Takes effect now | immediately | at once |
| 0–9 in names | numbers | digits |
| The per-bucket CORS setting | Browser access (CORS) | Browser access (alone) |
| The person using the UI | you | this key (when a user is signed in) |
| Someone without a key or user | people without an account | people without a key |

Technical names with no plain equivalent stay: webhook, presigned URL, API
key, quota, SHA-256.

## Patterns

- **Sentence case** for titles, buttons, labels and column headers.
- **Toasts**: the thing, then a past-tense verb, naming it when there is
  one: `Bucket “invoices” deleted`, `API key “billing” revoked`,
  `Share link revoked`.
- **Confirm dialogs**: the title asks the question (`Delete bucket
  “invoices”?`), the body gives the consequence in one or two sentences,
  the button repeats the verb (`Delete bucket`). End irreversible ones with
  `This can't be undone.`
- **Errors**: what happened, then what to do (`This link has expired. Ask
  the person who sent it for a new one.`). Never show an error code; show
  the request ID only as small detail (`Request ID 01J…`).
- **Help under a field**: one sentence, at most two.
- **Placeholders**: an example value with no "e.g." (`priya`,
  `https://app.example.com`). In running text, write "for example".
- **Quotes** around names the user chose: curly quotes, `“invoices”`.
- **Numbers**: `3–63` with an en dash; sizes as `5 GB`, `10 MB`.

## Where messages come from

- API errors: `internal/server/errors.go`. Each row of `errorMapping` has a
  lead sentence; `classify` appends the error's detail after a colon. The
  `code` is the contract; the message may be reworded.
- The UI can replace a code's message with one that fits its context:
  `MESSAGES` in `internal/webui/static/js/api.js`. Use it when the UI knows
  more than the API (for example "Reload the list to check" for
  `CommitUncertain`).
- The public upload page has its own short messages in `js/upload-page.js`,
  written for someone who has never seen BlobFish.
