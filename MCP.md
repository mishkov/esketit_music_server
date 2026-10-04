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
| `get_catalog_item` | entityType + entityId. Full visible entity metadata and relationship IDs. |
| `list_submissions` | Optional status/entityType/updatedSince/page/pageSize. Full owned records, feedback, revision, dependencies and allowedActions, including terminal submissions. |
| `get_submission` | submissionId. Current entity, full feedback history, revision, dependencies and allowedActions. |
| `get_upload` | uploadToken. Owned media type, size, SHA-256 of input bytes and claimedSubmissionId (null until used). |
| `submit_author` | requestId + currentName; optional photoUploadTokens. |
| `submit_album` | requestId + title + authorIds + releaseDate (RFC3339); optional coverUploadToken/additionalInfo. Release date is required by the existing backend. |
| `submit_track` | requestId + name + authorIds + albumId + audioUploadToken; optional albumOrder/additionalInfo/sourceMetadata. |
| `update_author_submission` | requestId + submissionId + expectedRevision + at least one changed author field. |
| `update_album_submission` | requestId + submissionId + expectedRevision + at least one changed album field. |
| `update_track_submission` | requestId + submissionId + expectedRevision + at least one changed track field. |
| `resubmit_submission` | requestId + submissionId + expectedRevision. changes_requested → pending_review, subject to dependency validation. |
| `cancel_submission` | requestId + submissionId + expectedRevision. Eligible owned pending submissions; dependent items block cancellation. |

Submission responses include both `submissionId` and `entityId`. Use entity IDs
in authorIds/albumId; use submission IDs for workflow tools. Edits require
`changes_requested`; omitted fields are preserved. Clear optional metadata or
photos with an empty array. Clear a cover with `coverUploadToken: null`.
Required names, references and dates cannot be cleared. Album order is zero-based;
omitting it appends new/moved tracks and preserves existing track positions.
Albums with tracks derive their author list from those tracks. Photo tokens replace
the entire photo list. Audio tokens replace the recording. Upload tokens are
owned by the selected user and may be claimed only once.

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
the complete stream, with a five-minute validation deadline. Images accept
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
