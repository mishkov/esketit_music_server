# Catalog submission MCP

The server exposes Streamable HTTP MCP at `/mcp` using the official Go MCP SDK.
It is stateless: workflow state, revisions, uploads, and successful retry
receipts live in SQLite, and survive client disconnects and server restarts.
Reviewer decisions remain in the existing review API and console.

## Setup

1. Set a dedicated `MCP_ACCESS_TOKEN` with at least 32 characters on the server.
   Keep it separate from `AUTH_SECRET`; use HTTPS for remote connections.
   If using a reverse proxy that preserves its public Host header, set
   `MCP_PUBLIC_URL=https://your-server.example` (server base URL without `/mcp`).
   This permits that exact Host while retaining host validation, and supplies
   an absolute upload endpoint in get_context. Explicit CORS_ALLOW_ORIGIN is
   also honored for browser MCP requests.
2. In console **Settings → AI Agent / MCP**, select an acting user and edit the
   workflow/upload guidance. This requires `access_control.manage`.
3. Configure the agent's Streamable HTTP client with the server URL ending in
   `/mcp` and `Authorization: Bearer <MCP_ACCESS_TOKEN>`.
4. Give the agent an HTTP client or shell capable of uploading local files.
   MCP tool calls do not read files on the agent's computer.

An empty/short credential disables network MCP access. A null acting user
blocks tools and uploads. Every operation checks the selected user's current
permissions. The agent cannot select another identity or edit settings.
The bearer credential grants access only to the MCP tools and upload endpoint,
not the ordinary REST administration APIs. Clients must support an explicitly
configured Bearer header; this release does not provide OAuth discovery/login.

## Tools

Use `tools/list` for authoritative JSON schemas, including required fields and
limits. Every result supplies `structuredContent` and the equivalent text:
`{"ok":true,"data":{...}}` or
`{"ok":false,"error":{"code":...,"message":...,"fields":[],"retryable":false,"recovery":...}}`.

| Tool | Contract |
| --- | --- |
| `get_context` | No arguments. Identity, permission codes, configurationVersion, editable guidance, generated upload contract, and enforced workflow rules. |
| `search_catalog` | query; optional entityType/page/pageSize. Published and owned pending authors/albums/tracks, case-insensitive substring matching. No automatic fuzzy identity decisions. |
| `get_catalog_item` | entityType + entityId. Visible metadata and relationship IDs. Albums include trackIds and orderRevision from one snapshot; invisible pending tracks are excluded. |
| `list_submissions` | Optional status/entityType/updatedSince/page/pageSize. Full owned records, feedback, revision, dependencies and allowedActions, including terminal submissions. |
| `get_submission` | submissionId. Current entity, full feedback history, revision, dependencies and allowedActions. |
| `get_upload` | uploadToken. Owned media type, size, SHA-256 of input bytes and claimedSubmissionId (null until used). |
| `submit_author` | requestId + currentName; optional photoUploadTokens. |
| `submit_album` | requestId + title + authorIds + releaseDate (RFC3339); optional isPublished (defaults to true)/coverUploadToken/additionalInfo. Release date is required by the existing backend. |
| `submit_track` | requestId + name + authorIds + albumId + audioUploadToken; required trackNumber; optional additionalInfo/sourceMetadata. |
| `update_author_submission` | requestId + submissionId + expectedRevision + at least one changed author field. |
| `update_album_submission` | requestId + submissionId + expectedRevision + at least one changed album field. |
| `update_track_submission` | requestId + submissionId + expectedRevision + at least one changed track field. |
| `resubmit_submission` | requestId + submissionId + expectedRevision. changes_requested → pending_review, subject to dependency validation. |
| `reorder_album_tracks` | requestId + albumId + tracks + expectedOrderRevision. Reorder every track in an approved album; requires albums.reorder_tracks. Cannot repair pending albums. |
| `submit_album_track_order` | requestId + albumId + tracks + expectedOrderRevision; optional reason. Creates a pending order proposal without changing the album; requires albums.submit_track_order. |
| `update_album_track_order_submission` | requestId + submissionId + expectedRevision; patch tracks/expectedOrderRevision/reason on an owned changes_requested proposal. |
| `cancel_submission` | requestId + submissionId + expectedRevision. Eligible owned pending submissions; dependent items block cancellation. |

Submission responses include both `submissionId` and `entityId`. Use entity IDs
in authorIds/albumId; use submission IDs for workflow tools. Edits require
`changes_requested`; omitted fields are preserved. Clear optional metadata or
photos with an empty array. Clear a cover with `coverUploadToken: null`.
Required names, references and dates cannot be cleared. Track numbers are positive and one-based. New tracks and album moves require an explicit
`trackNumber`; metadata patches preserve it when omitted. Gaps remain unchanged.
Occupied numbers return `track_number_conflict`; never retry without the number.
Albums with tracks derive their author list from those tracks. Photo tokens replace
the entire photo list. Audio tokens replace the recording. Upload tokens are
owned by the selected user and may be claimed only once.

Album `isPublished` controls whether an approved album is released content.
New MCP albums default to `true`; supply `false` to intentionally keep a draft.
Updates preserve this setting when omitted. Review approval independently sets
`publicationStatus` to `published` and never overrides an explicit draft setting.
Before approval, even an album with `isPublished: true` remains private under the
normal review visibility rules. Existing albums created with the old MCP default
retain their stored setting; an administrator can enable Published in the console
for affected approved albums.

`reorder_album_tracks` works only when `publicationStatus` is `published`
(approved), independently of `isPublished`. It requires the dedicated
`albums.reorder_tracks` permission, granted to the admin role by default.
Read the album with `get_catalog_item` and fetch its tracks to see their persisted
`trackNumber` values, then send:

```json
{
  "requestId": "album-42-order-1",
  "albumId": 42,
  "tracks": [{"trackId": 103, "trackNumber": 1}, {"trackId": 101, "trackNumber": 4}, {"trackId": 102, "trackNumber": 10}],
  "expectedOrderRevision": 4
}
```

The success envelope contains `data: {"albumId":42,"tracks":[{"trackId":103,"trackNumber":1},{"trackId":101,"trackNumber":4},{"trackId":102,"trackNumber":10}],
"orderRevision":5}`. Supply every current track exactly once with a distinct positive trackNumber. Duplicates, missing,
additional, unknown, nonpositive, and foreign-album IDs fail without normalization.
The operation changes only order: it cannot add/remove/move tracks or repair a
pending album. It does not change pending-submission editing rules. Distinct positive numbers may contain gaps.

Visibility rules still apply. An approved album may contain pending tracks;
album reads exclude tracks invisible to the acting user, and reordering is
blocked with `invalid_state` if the full membership is not visible. Never submit
a filtered subset. Owned pending tracks may be included in a complete order.
A stale revision returns `stale_revision` with field `expectedOrderRevision`;
read the album again and reassess before using a new requestId. Unapproved albums
also return `invalid_state`.

Album `orderRevision` starts at 1 for existing and new albums. Every membership
or order change advances it, including legacy track/album writes, movement,
deletion, cancellation, and rejection. Metadata-only changes and an identical
order with the current revision leave it unchanged. Submission `revision` is
separate. Permission checks, validation, revision comparison, update and persisted
receipt insertion share one transaction. Identical retries return the original
receipt even after later order changes or a restart; changed payloads require a
new requestId. This tool does not automatically repair existing album data.

`additionalInfo` accepts the existing metadata objects; text records require
`type: "text"`, `title`, and `text`. External links require
`type: "external_link"`, `provider`, and `url`. `sourceMetadata` entries require
`provider` and a nonempty `identity` object, with an optional source `url`.

## Media uploads

`POST /api/mcp/uploads`, using the same dedicated Bearer credential:

```sh
curl --fail-with-body \
  -H "Authorization: Bearer $MCP_ACCESS_TOKEN" \
  -H "Idempotency-Key: stable-upload-request-id" \
  -F kind=audio \
  -F file=@./track.mp3 \
  https://your-server.example/api/mcp/uploads
```

Exactly one `kind` text field and one `file` binary part are accepted.
Kinds: audio, author_photo, album_cover. Audio supports MP3 up to 512 MiB;
validation checks MP3 format and uses `FFMPEG_BINARY` (default ffmpeg) to decode
the complete stream, with a five-minute validation deadline. The same validation
also applies to regular song uploads and review-submission audio. Accepted audio
keeps its original name in metadata, while stored and published filenames use
`.mp3` so streaming responses have `audio/mpeg`. Corrupt audio is rejected before
any upload token is registered. If FFmpeg is unavailable, uploads return 503 and
can be retried; this is not reported as invalid input.

Telegram imports automatically convert supported non-MP3 audio to validated MP3
and normalize filenames, preserving valid MP3 bytes without re-encoding. YouTube
imports validate their transcoded MP3 output before publication.

Images accept
JPEG/PNG/GIF up to 10 MiB and 4096 × 4096 pixels, and are re-encoded to strip
metadata/trailing data (GIF becomes static PNG). Author submissions accept up
to ten photos; albums accept one cover. The total multipart body is bounded by
512 MiB + 1 MiB of overhead. The agent must use the limits in get_context.

Success returns an uploadToken, kind, originalName, detected/stored mediaType,
sizeBytes, input SHA-256, createdAt and claimedSubmissionId. Use the token in an
MCP submission tool. Audio remains private until approved; existing review
approval, rejection and cancellation handle publication/deletion. Images use
the existing public, unguessable asset paths. Unclaimed upload tokens do not
expire automatically in this release.

## Retry and concurrency contract

Each write requires a stable requestId (1–128 characters). HTTP uploads use
Idempotency-Key. These share one namespace per acting user: allocate a unique
ID for each operation. Receipts have no automatic expiry.

- Same user, ID, operation and payload: return the original successful result,
  even if that submission subsequently changed. Fetch get_submission/get_upload
  for current state.
- Same user and ID with another operation/payload: request_conflict (HTTP 409
  for uploads). Upload payload identity includes kind, original filename, size
  and SHA-256; multipart boundary differences do not matter.
- A failed operation creates no successful receipt. Corrected work uses a new
  ID; an uncertain/temporarily unavailable operation retries its original ID
  and payload.
- Idempotency lookup, authorization, expectedRevision validation, catalog
  mutation, token claim and receipt insertion commit in one transaction.
- Files replaced or cancelled through MCP are removed after that transaction
  commits. Failed transactions preserve the original media. Startup repairs
  existing unreferenced audio staging files.

Read fresh context at session start and after configuration/permission errors.
Poll list_submissions for feedback; updatedSince is an inclusive RFC3339 time
filter. Use an overlap when polling, deduplicate by submissionId/revision, and
page through all results. Rejected/cancelled snapshots remain inspectable.

## Console settings API

GET/PATCH `/api/mcp/settings` uses ordinary user access-token authentication and
requires `access_control.manage`. It returns:

```json
{
  "actingUserId": 123,
  "workflowGuidance": "Submission standards and operating instructions",
  "uploadGuidance": "Additional media preparation instructions",
  "version": 2,
  "credentialConfigured": true
}
```

PATCH requires expectedVersion and at least one changed field. Omitted fields
are preserved; actingUserId null disables the agent. Guidance fields accept
empty text and are limited to 32,000 UTF-8 bytes each. Stale versions return
409 without changes. Settings changes are audited. CredentialConfigured is
read-only; no credential is returned. Editable text guides the agent while
permissions, upload limits and lifecycle constraints remain server-enforced.

## Reviewed album track order proposals

Use `submit_album_track_order` for changes that need human approval. The deployed
`reorder_album_tracks` tool and direct HTTP API apply the same explicit number assignments immediately.
The proposal permission `albums.submit_track_order` is granted to admin and
listener roles by default; it does not grant immediate reorder access.

Read the album's complete visible `trackIds` and `orderRevision` using
`get_catalog_item`, then submit every current member exactly once in the intended
order as `{trackId, trackNumber}` assignments. Only approved albums (`publicationStatus: published`, independent of
`isPublished`) are eligible. Hidden pending membership prevents submission.
Do not omit requested ordering to bypass an error.

Each proposal has its own `submissionId`, `entityType: album_track_order`, and
`entityId` pointing to the target album. One pending_review or changes_requested
proposal is allowed per requester/album; proposals from other requesters can
coexist. Filter `list_submissions` by `entityType: album_track_order` and use
`get_submission` for feedback and allowedActions. Proposal entities include:

- `baseTracks`, proposed `tracks`, `expectedOrderRevision`, and `reason`;
- `currentTracks`, `currentOrderRevision`, `targetAvailable`, and visible
  `trackDetails` summaries (`id`/`name`) for comparison;
- `appliedOrderRevision` once approved. The saved snapshot remains historical.
- `albumTitle`, `coverImagePath`, and `outdated` for review presentation. An active
  proposal is outdated when the album is unavailable, its revision changed, or
  its saved `baseTracks` no longer matches `currentTracks`. Reviewers can request
  changes or reject it, but cannot approve it.

Review uses existing reviewer endpoints, permissions, and requester leases;
there are no new MCP review tools. Approval rechecks current membership and the
base track-number assignments and ordering revision in the same transaction as
the order change and decision.
If a direct reorder, track insertion/removal, or competing approval made the
proposal stale, approval returns conflict and leaves it pending and the album
unchanged. Request changes, fetch the latest album, patch the proposal with fresh
`expectedOrderRevision` and intended `tracks`, then resubmit with the latest
submission revision. Never silently merge a stale proposal.

Edits preserve omitted fields; an empty reason clears it. Rejecting, cancelling,
or requesting changes affects only the proposal and feedback, never the album's
publication, ownership, tracks, or media. Repeat the same requestId and payload
after timeouts; use a new requestId for changed work. Idempotency receipts persist.

HTTP equivalents: `POST /api/catalog-submissions/album-track-orders` and
`PUT /api/catalog-submissions/album-track-orders/{submissionId}`. Updates require
`expectedRevision`; creation does not. Existing list, resubmit, cancel, and
review endpoints also handle this submission type.

Persistent numbering replaces the previous insertion index contract. `albumOrder`
and album write `trackIds` are rejected; direct reorder and proposals require
`tracks: [{trackId, trackNumber}]`. Album read `trackIds` remains a derived list.
The automatic database migration preserves existing relative order as 1..N;
repair previously incorrect ordering with explicit assignments.

## Synced lyrics and published-track corrections

`submit_track` accepts optional `lyrics` in the same format as `PUT /api/tracks/{id}/lyrics`.
`update_track_submission` accepts the same field; omitting it preserves existing lyrics.
Track creation and lyrics validation are atomic: invalid lyrics do not claim the upload or create a track.
`get_submission`, `list_submissions`, and the reviewer queue include a top-level `lyrics` object for track submissions.

```json
{
  "type": "synced",
  "lines": [
    {"startMs": 1000, "endMs": 2800, "text": "Example first line"},
    {"startMs": 3000, "text": "Example final line"}
  ]
}
```

Use sourced lyrics and timestamps. Starts must strictly increase; lines cannot overlap.
Every line except the last requires `endMs`. Synced lyrics cannot include `plainText`.

For an already published track, use `submit_track_lyrics` with `requestId`, `trackId`,
and `lyrics` (plain or synced). It creates a `track_lyrics` review proposal without
changing the track, audio, or current public lyrics. Requires `tracks.submit`.
After requested changes, call `update_track_lyrics_submission` with `submissionId`,
`expectedRevision`, and optional replacement `lyrics`, then `resubmit_submission`.
Updating refreshes the proposal's published baseline. Approval rejects stale proposals
when published lyrics or the audio path changed. Rejecting or cancelling a proposal
leaves the published track and lyrics intact. Normal ownership, leases, revisions,
and persistent request receipts apply. One active proposal per requester and track;
terminal proposals do not prevent later corrections.

REST equivalents: `POST /api/catalog-submissions/track-lyrics` and
`PUT /api/catalog-submissions/track-lyrics/{submissionId}`. Reviewers use the normal
review queue and decision endpoints. The console previews the proposed lyrics
against the published audio for corrections and staged audio for new tracks.


## Changes to published tracks

Use `submit_track_update` to propose a correction to any published track.
It requires `tracks.submit`, not direct catalog-edit permission. The published
track, audio, playlists and lyrics remain live until human approval.

Search and inspect the track first. Supply `trackId` and any changed fields:
`name`, `authorIds`, `albumId`, `trackNumber`, `additionalInfo`,
`sourceMetadata`, `lyrics` or `audioUploadToken`. Identity, creation time and
publication/review state are immutable. Changing albums requires an explicit
destination number; occupied numbers are rejected. Retain original source
identifiers when adding replacement provenance.

Omitted fields are preserved. Empty metadata arrays clear those lists.
`lyrics: null` removes lyrics. Upload replacement audio to
`POST /api/mcp/uploads` first and pass its owned unused token; remote URLs and
file paths are not accepted. An empty `audioUploadToken` on a correction
removes a proposed replacement and uses the baseline audio. Tokens are
consumed once and cannot be reused through the legacy submission API.

Example MCP arguments:

```json
{
  "requestId": "replace-track-2981-audio",
  "trackId": 2981,
  "audioUploadToken": "<owned-upload-token>",
  "reason": "Replace compressed audio with the verified studio recording."
}
```

The `track_update` submission retains `before`, `after`, `changedFields` and
`reason`. Its current view adds `current`, `targetAvailable`, `isOutdated`,
and artist/album names. The console compares both versions with separate
authenticated audio players.

Approval checks the published track and lyrics against the captured baseline.
A changed/deleted target, newly occupied number or unavailable dependency
prevents approval. After reviewer feedback, inspect `before`, `after` and
`current`, then call `update_track_update_submission` using the fresh
`expectedRevision`. Set `refreshBaseline: true` only after reviewing concurrent
changes: rebasing copies only proposed changed fields onto the latest data.
Then call `resubmit_submission`. Ownership, review leases, feedback,
cancellation and idempotent request rules are unchanged. Rejection and
cancellation discard private audio while retaining comparison history.

Equivalent REST endpoints (authenticated user, no `requestId` field):

- `POST /api/catalog-submissions/track-updates`
- `PUT /api/catalog-submissions/track-updates/{submissionId}`
- `GET /api/catalog-submissions/track-updates/{submissionId}/audio` (private
  preview with range support; reviewers send a valid `X-Review-Lease`)

Requires schema migration 14 and the matching console version.
