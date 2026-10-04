# Persistence architecture

SQLite is the durable source of truth. `trackStore` remains the application
facade so routing, authentication, and import handlers keep their existing
signatures, while persistence is delegated through these domain boundaries:

- `UserRepository`
- `RefreshSessionRepository`
- `AuthorRepository`
- `CatalogRepository` (albums and tracks share transaction requirements)
- `PlaylistRepository`
- `LyricsRepository`
- `ReadRepository` (cross-aggregate filtering, pagination, and projections)
- `AccessControlRepository` (roles, permissions, assignments, and audit events)
- `MCPRepository` (agent settings, retry receipts, staged media metadata and scoped agent reads)
- `CatalogSubmissionRepository` (submissions, feedback, rating events, review
  leases, and staged-upload claims)

The SQLite implementations accept `context.Context` and contain SQL encoding,
row scanning, constraint translation, and driver-specific behavior. The unit of
work supplies all repositories bound to one transaction without exposing
`*sql.Tx` to handlers or domain workflows.

## Reads and writes

`trackStore` retains only immutable service dependencies. Persistent entities
are read from repositories for each operation. Algorithms that correlate
multiple denormalized rows may construct a request-local `domainState`; it is
discarded when the operation returns and is never an application cache.

Normal API reads use scoped SQL queries. Filters, counts, ordering, and
pagination execute in SQLite before rows are decoded. Multi-table responses
run inside one read-only unit-of-work transaction, so their tracks, albums,
authors, playlists, and preference flags come from one database snapshot.
Only operations whose result inherently depends on the complete catalog, such
as autoplay candidate selection and import suggestion scoring, load broad
request-local state; those loads also use a consistent read transaction.

Mutations issue explicit inserts, updates, and deletes for affected rows only.
Operations spanning domains use repositories bound to one unit-of-work
transaction. SQLite rollback replaces the former map snapshot/restore logic.

## Access control

Authorization is permission-based. Users may have any number of roles, roles
may have any number of permissions, and effective permissions are the distinct
union across every assigned role. These relationships are stored in
`user_roles` and `role_permissions`; authorization never depends on a role
embedded in the user row or on an in-memory permission cache.

Permission definitions correspond to concrete server actions and are seeded by
schema migration. They are read-only through the management API because adding
an arbitrary database permission cannot create an authorization check in code.
Roles and both assignment relationships are managed transactionally.

The built-in `admin` and `listener` roles establish registration defaults. They
cannot be renamed or deleted, but their permission assignments are editable.
Every access-control mutation is recorded in
`access_control_audit_events`. Transactions reject any change that would leave
the system without a user who has `access_control.manage`.

## Catalog submission workflow

Authors, albums, and tracks have an explicit publication lifecycle. Published
rows are visible to everyone; `pending_review` and `changes_requested` rows are
visible through normal catalog, search, playlist, and autoplay reads only to
the user identified by `requested_by_user_id`. Direct catalog-management
operations only mutate published rows. Pending rows are created and changed
through the submission service, so review state cannot be bypassed accidentally.

`catalog_submissions` is the durable workflow record. It retains the latest
entity snapshot after rejection or cancellation. Rejected authors and albums
that are still referenced remain as hidden `rejected` rows; dependent tracks
cannot receive review decisions. Feedback and import-rating changes are
append-only rows. Authors and albums must be approved before dependent tracks.
A track approval records the +10 approval event and optional +5 lyrics event.
Review penalties are explicit negative events and default to zero.

Review ownership is stored in `catalog_review_leases`, not process memory. A
unique requester and unique reviewer constraint guarantee that two reviewers
cannot review the same requester and that one reviewer cannot hold two queues.
Lease tokens are bound to both users, expire after ten minutes, and are renewed
by heartbeat or successful review decisions.

Uploaded submission audio is stored below the configured songs directory in
`.catalog-submissions` and is not exposed by the public song endpoint. The
requester can stream it immediately; the active reviewer can stream it with the
lease token. Approval publishes it into the public songs directory, while
rejection or cancellation removes it. Publication uses a same-filesystem hard
link so the staged copy survives until the database commit succeeds. On
rollback the public link is removed; after commit the staged link is removed.
Startup deletes unreferenced staging files and any public hard link still tied
to an active staged upload after a crash between those steps.

## Transaction boundaries

- User creation advances the user and playlist counters, inserts the user, and
  creates both system playlists in one transaction.
- Track creation advances its counter and updates the track and album order in
  one transaction.
- Track movement updates the track plus its old and new album rows atomically.
- Track deletion updates album membership and playlist unavailable-track data,
  and deletes lyrics with the track in one transaction.
- Multi-playlist membership and preference changes are atomic.
- Refresh rotation removes expired sessions, removes the old session, and
  inserts the replacement as one session-domain transaction.
- Lyrics and their embedded synchronized lines are written atomically.
- Role assignments, permission assignments, their lockout check, and audit
  event insertion commit atomically.
- Submission creation writes the pending catalog row, workflow snapshot, and
  staged-upload claim atomically.
- Approval validates the review lease, publishes the entity and its pending
  dependencies, and records rating events atomically.
- Feedback, its optional rating penalty, the entity lifecycle, and the
  submission lifecycle change atomically.

## ID allocation

Existing IDs and `store_metadata` counters are retained. `AllocateID` advances
a counter with `UPDATE ... RETURNING` inside the same transaction as the entity
insert. Startup repairs counters upward from table maxima, so deleting the
highest-numbered row never makes its ID reusable.

Import files are staged and validated before publication. The filesystem link
is published immediately before the database transaction; failures leave the
database unchanged and the import service removes the published file.

## Startup and schema changes

`schema_migrations` records ordered SQLite migrations. Each migration and its
history row commit atomically. Startup runs narrow transactional repairs for ID
counters, expired sessions, missing system playlists, and relationship checks.
It does not hydrate the database into memory. There is no JSON-file persistence
or startup import path.

The modernc SQLite DSN applies `foreign_keys(1)` and the busy timeout to every
connection. Several existing relationships remain embedded in JSON columns and
therefore cannot use SQLite foreign keys; startup validates those relationships
with explicit SQL. New normalized relationship tables should declare foreign
keys in their migrations.

## MCP transaction integration

MCP uses the same submission workflows through a request-local trackStore facade
whose joined unit of work supplies repositories already bound to the outer
transaction. It starts no nested SQL transactions and caches no application
state. Identity/permission checks, current revision validation, upload claims,
catalog changes, and the successful retry receipt commit together. A failed
receipt insert rolls back the submission. File deletions from replacement and
cancellation are collected on the request-local facade and run after commit.

Every submission row has an integer revision incremented by the repository on
all lifecycle/snapshot updates, including ordinary HTTP review decisions. MCP
patches compare expectedRevision inside their write transaction. Settings use
a separate version with compare-and-swap updates and access-control audit rows.
Retry receipts and media tokens are bound to the selected user through foreign
keys. They intentionally have no automatic expiry; get_submission/get_upload
return current state while replayed writes return their original receipt.
