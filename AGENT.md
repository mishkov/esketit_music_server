# Agent conversations

The Go API authenticates `/api/agent/*` with the existing app session and checks
the dedicated `agent.use` permission on every request. The Python service owns
conversation state, PostgreSQL checkpoints, tool receipts, questions, item queues,
review monitoring and the notification outbox. Model/API and MCP credentials never
reach Flutter. The current MCP has one configured acting user; Agent access is
limited to that same user even if another account receives `agent.use`.

## Deployment

1. Choose the existing MCP acting user in the console and assign `agent.use` using
   Access Control. The feature migration does not grant it to existing roles.
2. Generate an independent shared secret with at least 32 characters. Set
   `AGENT_SERVICE_TOKEN` identically on Go and Python, and set
   `AGENT_SERVICE_URL=http://127.0.0.1:8312` on Go.
3. In `agent/`, install `pip install .` inside a virtual environment. Configure
   `AGENT_DATABASE_URL=postgresql+psycopg://...`, `AGENT_SERVICE_TOKEN`,
   `ESKETIT_URL` (catalog origin, without `/api`), `ESKETIT_MCP_TOKEN`,
   `OPENAI_API_KEY`, `OPENAI_MODEL`, and `AGENT_MEDIA_ROOT`.
   The default model is `gpt-5.4-mini`; no expensive-model fallback is configured.
4. Run `uvicorn esketit_agent.api:application --factory --host 127.0.0.1
   --port 8312 --no-access-log` under a service supervisor. Keep the internal
   service private. Use a single ASGI process; durable leases protect requests
   from duplicate claims during recovery. PostgreSQL must be backed up along
   with the catalog database and shared media directory.
5. Alternatively use `agent/compose.yaml` after supplying its environment.
   Add `-f compose.push.yaml` only after configuring Firebase server credentials;
   base chat does not require Firebase. Install stdio MCP executables in a
   derived image, or connect to separately managed HTTP MCP servers.

`AGENT_MCP_SERVERS` is a JSON object mapping a server name to either
`{"url":"https://.../mcp","headers":{...}}` or
`{"command":"...","args":[...],"env":{...}}`. All discovered tools are made
available. Esketit is configured separately and its connection cannot be
overridden. Keep this configuration server-side. For `ytm-mcp`, make downloaded
files available beneath the same `AGENT_MEDIA_ROOT`; local paths from a remote
machine cannot be uploaded unless its files are shared with the agent service.

The agent's `upload_media` tool validates the shared-directory boundary and
uploads bytes through Esketit's dedicated authenticated HTTP upload endpoint.
It does not put media bytes, base64 or local filesystem paths in MCP submissions.

## Behavior and recovery

- Drafts enter the durable queue only when a message is sent.
- Requests touching the same item wait in arrival order, including paused work
  and questions. Unrelated items remain eligible for execution.
- A queued request refreshes the item and prior review outcome before acting.
  Existing pending submissions created outside chats are also checked.
- Stop preserves work. In-flight operations settle before the state becomes
  Paused. Resume continues the same checkpoint; sending `resume` also works.
- Cancel abandons remaining agent steps; pending submissions continue blocking
  dependent work until their review is resolved. Completed changes are retained.
- Review checks run every ten minutes with no model invocation when unchanged.
  Clear corrections resume the agent; ambiguous feedback uses durable questions.
- Tool receipts and stable Esketit request identifiers prevent repeated writes
  after timeouts. Unknown outcomes of other write tools require reconciliation.
- Questions and completion use explicit `ask_user` and `finish_request` actions.
  Plain model text cannot silently finish a chat that still needs an answer.
- A steering message supersedes unexecuted calls from an earlier instruction.
- Notifications are retried from the durable outbox. Delivery can be repeated
  after a dispatcher crash; each notice carries a stable identifier.

## Notifications

Create a Firebase project, configure the matching iOS application and upload the
APNs authentication key in Firebase. Provide server credentials using
`GOOGLE_APPLICATION_CREDENTIALS` (outside Git), and follow the Flutter app's
`docs/agent-chat.md`. Without a Firebase project, chat and recovery remain usable,
but remote notification delivery cannot be verified.

## Verification

Run `go test ./...`, then `cd agent && pip install '.[test]' && pytest`.
The suite exercises ownership, safe request retries, item queueing, stop/resume,
questions across process restarts, and review dependencies. Run against PostgreSQL
as well as SQLite tests before deployment. Real push requires a signed iOS app,
configured Firebase/APNs and a registered device.
