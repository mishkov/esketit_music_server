# Deploy Esketit Agent on the existing VPS

This runbook assumes the existing Go API runs under systemd as
`esketit_music_server.service` on port 8080. The worker will run on the same VPS,
on loopback port 8312, with a separate PostgreSQL database. The catalog stays in
its existing SQLite database. Start with a fresh production agent database so
local test chats and device registrations do not carry over.

## 1. Check the host and deploy the Go API

SSH to your existing deployment host. Record the current revision, check free
space and confirm the existing service is healthy.

```bash
cat /opt/esketit_music_server/current/REVISION
sudo systemctl is-active esketit_music_server.service
curl -fsS http://127.0.0.1:8080/healthz
df -h /opt /var/lib
```

Check that your production deployment and off-host catalog/media backups are
current. Review the backend PR and wait for checks before merging it. The
existing production GitHub Actions workflow deploys Go when main changes. Its
deploy script takes and verifies a SQLite backup under
`/var/lib/esketit_music_server/backups/`, then starts the new binary. Migration
15 adds `agent.use` automatically, with no automatic grants. The Go release
archive contains only the binary and release metadata, so install the Python
worker separately in step 3.

After the deploy workflow succeeds, check `/healthz` again and record the new
`REVISION`. Leave the app PR unmerged while setting up and verifying the worker.

## 2. Create the PostgreSQL database and worker identity

On Debian/Ubuntu with Python 3.11 or later and sudo access, use the distro
PostgreSQL package or your existing local PostgreSQL instance. Use a supported
release. PostgreSQL 17 is the version used by this repository's Compose setup
and the local integration environment.

```bash
python3 --version
sudo apt-get update
sudo apt-get install --yes python3-venv postgresql postgresql-client
sudo systemctl enable --now postgresql
getent passwd esketit_agent >/dev/null || sudo useradd --system --user-group \
  --home-dir /var/lib/esketit-agent --shell /usr/sbin/nologin esketit_agent
sudo install -d -o root -g esketit_agent -m 0750 /etc/esketit-agent
sudo install -d -o esketit_agent -g esketit_agent -m 0750 \
  /var/lib/esketit-agent /var/lib/esketit-agent/media
sudo -u postgres createuser --pwprompt esketit_agent
sudo -u postgres createdb --owner=esketit_agent esketit_agent
psql -h 127.0.0.1 -U esketit_agent -d esketit_agent -W -c 'SELECT 1'
```

The two database creation commands are for first installation. Use a generated
hex password so it is safe in the database URL. Keep PostgreSQL private. Use
your existing installation if it already serves other applications.
[PostgreSQL role creation](https://www.postgresql.org/docs/current/app-createuser.html)

## 3. Install the matching Python worker

In a trusted checkout of the backend repo on your Mac, fetch the deployed Go
revision and archive the tracked worker files. Replace both placeholders.

```bash
git fetch origin main
revision='<deployed-server-revision>'
vps='<your-ssh-host-or-alias>'
git archive --format=tar.gz --output="/tmp/esketit-agent-$revision.tar.gz" \
  "$revision" agent
scp "/tmp/esketit-agent-$revision.tar.gz" "$vps:"
```

On the VPS, use the same revision. Keep each release in its own directory.

```bash
revision='<deployed-server-revision>'
agent_release="/opt/esketit-agent/releases/$revision"
sudo install -d -m 0755 "$agent_release"
sudo tar -xzf "$HOME/esketit-agent-$revision.tar.gz" -C "$agent_release"
sudo python3 -m venv "$agent_release/venv"
sudo "$agent_release/venv/bin/python" -m pip install "$agent_release/agent"
sudo ln -sfnT "$agent_release" /opt/esketit-agent/current
```

## 4. Configure credentials and the Go gateway

Generate a new random shared token using `openssl rand -hex 32` and store it
privately. Use the production Go service's existing `MCP_ACCESS_TOKEN` as the
worker's `ESKETIT_MCP_TOKEN`. If MCP was never configured on this VPS, configure
it with a separate token first. Keep the existing Go auth and catalog settings.

Create root-only files `/etc/esketit-agent/worker.env` and
`/etc/esketit-agent/gateway.env` with mode `0600`. Edit using `sudoedit`. Replace
every angle-bracket value. The worker's catalog origin has no `/api` suffix.

```bash
sudo touch /etc/esketit-agent/worker.env /etc/esketit-agent/gateway.env
sudo chmod 0600 /etc/esketit-agent/worker.env /etc/esketit-agent/gateway.env
sudoedit /etc/esketit-agent/worker.env /etc/esketit-agent/gateway.env
sudo install -d -m 0755 /etc/systemd/system/esketit_music_server.service.d
```

Worker environment:

```dotenv
AGENT_DATABASE_URL=postgresql+psycopg://esketit_agent:<db-password>@127.0.0.1:5432/esketit_agent
AGENT_SERVICE_TOKEN=<new-shared-service-token>
ESKETIT_URL=http://127.0.0.1:8080
ESKETIT_MCP_TOKEN=<production-go-mcp-access-token>
OPENAI_API_KEY=<funded-openai-project-key>
OPENAI_MODEL=gpt-5.4-mini
AGENT_MEDIA_ROOT=/var/lib/esketit-agent/media
AGENT_MCP_SERVERS={}
GOOGLE_APPLICATION_CREDENTIALS=/etc/esketit-agent/firebase-admin.json
```

Gateway environment:

```dotenv
AGENT_SERVICE_URL=http://127.0.0.1:8312
AGENT_SERVICE_TOKEN=<same-shared-service-token>
```

Transfer the existing Firebase Admin service-account JSON through SSH to a
private staging path on the VPS. Install it at the worker's path as
`root:esketit_agent` with mode `0640`, then remove the staging copy. This file
belongs on the server. The client uses the separate public Firebase app config.
Leave the `GOOGLE_APPLICATION_CREDENTIALS` line out until push is configured.

Add `/etc/systemd/system/esketit_music_server.service.d/agent.conf` to load
only the gateway settings. Preserve any existing drop-ins.

```ini
[Service]
EnvironmentFile=/etc/esketit-agent/gateway.env
```

At the public reverse proxy, keep routing `/api/agent/` to Go. Keep the worker
and PostgreSQL off public ports. If the proxy currently forwards every `/api/`
route, return 404 for public requests under `/api/agent-internal/`; the worker
uses that route over Go's loopback origin. Check the proxy config before reload.

## 5. Start both services and grant access

Create `/etc/systemd/system/esketit-agent.service`.

```ini
[Unit]
Description=Esketit Agent worker
Wants=network-online.target
After=network-online.target postgresql.service esketit_music_server.service

[Service]
User=esketit_agent
Group=esketit_agent
WorkingDirectory=/var/lib/esketit-agent
EnvironmentFile=/etc/esketit-agent/worker.env
ExecStart=/opt/esketit-agent/current/venv/bin/uvicorn esketit_agent.api:application --factory --host 127.0.0.1 --port 8312 --no-access-log
Restart=on-failure
RestartSec=5
TimeoutStopSec=150
UMask=0027
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

Use one worker process. It initializes its PostgreSQL tables and checkpoints on
startup. Restart Go once to load the new gateway settings.

```bash
sudo systemd-analyze verify /etc/systemd/system/esketit-agent.service
sudo systemctl daemon-reload
sudo systemctl restart esketit_music_server.service
sudo systemctl enable --now esketit-agent.service
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8312/healthz
sudo journalctl -u esketit-agent.service -n 40 --no-pager
```

In the existing console:

1. Open **Settings → MCP** and select the intended production acting user.
   Use a separate contributor identity with the catalog permissions it needs.
   Keep the review account separate so it can approve the agent's submissions.
2. In **Access Control → Roles**, create a role with `agent.use`. In **Users**,
   assign that role to your normal account and preserve its other roles.
   Production user IDs may differ from local IDs.
3. In a build from the app PR pointed at this API, sign in as your normal account. Check that Agent appears for it
   and stays hidden for an account without permission.

The optional downloader MCP is a separate setup. Leave
`AGENT_MCP_SERVERS={}` for the initial smoke test. The built-in web search
already works with the configured OpenAI key. If you later add a downloader,
make its output available to the worker under `AGENT_MEDIA_ROOT`.

## 6. Ship the app and verify notifications

Before merging the app PR, set the app repository's GitHub Actions variable
`FIREBASE_IOS_CONFIG_JSON` to the contents of your public
`firebase-config.ios-production.json`. It must use the production iOS app
entry and match `IOS_APP_IDENTIFIER` (default
`com.mishkov.esketitMusicApp`). Update the CI provisioning profile if needed so
it includes production Push Notifications. The worker Admin credential, the
production iOS Firebase entry and the signed app must use the same Firebase
project. The APNs key uploads already completed in Firebase can be reused.
[Firebase iOS setup](https://firebase.google.com/docs/cloud-messaging/ios/get-started)

Once ready, merge the app PR. Its main workflows deploy the web app and build
the signed TestFlight app. The standard web deploy supports chat. To enable
optional web push, build with the public web app config and
`FIREBASE_WEB_VAPID_KEY`, then deploy `firebase-config.js` next to
`firebase-messaging-sw.js` over HTTPS. Include the config in each deploy
artifact because the web workflow uses `rsync --delete`.
See the app repo's `docs/agent-chat.md` for the public config format.

On a real iPhone, install the new TestFlight build, sign in and tap
**Agent → Enable notifications**. Start a research-only chat, for example:

> Find the original release year of Kavinsky's Nightcall and cite a source.
> Research only. Leave the catalog unchanged.

Background the app. Verify the completion alert opens the chat and its source
link works. Then have the agent ask a choice question, restart the worker and
reopen the chat to confirm the question persists. Use a test submission for the
review flow. Verify a reviewer can approve the MCP user's submission and the
agent reports the result on its next review check, normally within ten minutes.

## 7. Back up and roll back

Back up the production agent PostgreSQL database separately with
`pg_dump -Fc`, plus the worker media directory and protected configuration.
Keep those alongside the existing SQLite/media backup plan. Confirm you can
restore them before relying on the agent for work in progress.

For a worker upgrade, stage a new release as in step 3, back up PostgreSQL,
stop the worker, update `/opt/esketit-agent/current`, start it and run the smoke
checks. To turn off chat while preserving state, stop the worker, move the
Go `agent.conf` drop-in out of its service drop-in directory, reload systemd
and restart Go. Reinstall that drop-in to re-enable the feature. The Go deploy
script rolls back the binary if its health check fails and retains the
pre-deploy SQLite backup for manual recovery. Restoring an old database loses
writes since that snapshot, so use it only when necessary.
