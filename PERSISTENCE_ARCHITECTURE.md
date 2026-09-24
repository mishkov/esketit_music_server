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
