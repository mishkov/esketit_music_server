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

The SQLite implementations accept `context.Context` and contain SQL encoding,
row scanning, constraint translation, and driver-specific behavior. The unit of
work supplies all repositories bound to one transaction without exposing
`*sql.Tx` to handlers or domain workflows.

## Writes and compatibility cache

Existing handlers still read through `trackStore`'s in-memory maps. These maps
are a compatibility cache, not an independently persisted data model. A write
holds the store mutex until its SQL transaction commits, compares the affected
domain cache with current repository rows, and issues only the necessary
`INSERT`, `UPDATE`, or `DELETE` statements. A failed transaction restores the
cache snapshot before releasing the mutex. Unrelated repositories are not
opened for writes.

This bridge avoids a simultaneous HTTP-layer rewrite. A future step can replace
map-backed query methods with repository query methods without changing the
mutation or transaction boundaries introduced here.

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

## ID allocation

Existing IDs and `store_metadata` counters are retained. Creation performs a
compare-and-set counter advance from the facade's expected value inside the same
transaction as the inserted rows. A stale process therefore fails before it can
overwrite a row allocated by another process. Counters never move backward.
Deleting a highest-numbered row does not make its ID reusable.

If an imported track commits but audio publication fails, the catalog insert is
compensated with a second transaction while the already-reserved ID remains
consumed. This is intentional: SQLite cannot roll back a filesystem operation,
and non-reuse is safer than trying to rewind the counter.

## Startup and schema changes

`schema_migrations` records ordered SQLite migrations. Each migration and its
history row commit atomically. Startup loads repository rows directly into the
compatibility cache, validates relationships, applies domain normalization,
and reconciles only rows whose normalized representation changed. There is no
JSON-file persistence or startup import path.

The modernc SQLite DSN applies `foreign_keys(1)` and the busy timeout to every
connection. Existing catalog tables are intentionally not retrofitted with
foreign keys in this refactor; future RBAC migrations can safely create foreign
keys referencing `users(id)`.

Operational upgrade and rollback instructions are in
[`SQLITE_MIGRATION_GUIDE.md`](SQLITE_MIGRATION_GUIDE.md).
