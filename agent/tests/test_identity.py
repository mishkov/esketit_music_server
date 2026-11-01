from dataclasses import replace
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient
from langgraph.checkpoint.sqlite import SqliteSaver
from test_conversations import FakeConnections, create
from test_review_and_tools import prepare

from esketit_agent.api import create_app
from esketit_agent.notifications import Notifications
from esketit_agent.runtime import Runtime
from esketit_agent.store import Device, Notice, Store
from esketit_agent.tools import Connections


def test_authorization_uses_chat_owner_but_keeps_mcp_actor(monkeypatch, tmp_path):
    _, settings, _ = prepare(tmp_path)
    connection = Connections(settings)
    identities = []
    connection.call = lambda *args: {"data": {"actingUserId": 2}}

    def access(url, **arguments):
        identities.append(url)
        return SimpleNamespace(status_code=200)

    monkeypatch.setattr("esketit_agent.tools.httpx.get", access)
    assert connection.authorize(1)["actingUserId"] == 2
    assert identities == ["http://localhost/api/agent-internal/users/1"]
    monkeypatch.setattr(
        "esketit_agent.tools.httpx.get", lambda *args, **kwargs: SimpleNamespace(status_code=403)
    )
    with pytest.raises(PermissionError):
        connection.authorize(1)


@pytest.mark.parametrize("actor", [None, 0, -1, True, "2"])
def test_context_requires_a_valid_mcp_actor(tmp_path, actor):
    _, settings, _ = prepare(tmp_path)
    connection = Connections(settings)
    connection.call = lambda *args: {"data": {"actingUserId": actor}}
    with pytest.raises(PermissionError):
        connection.context()


class ServiceConnections(FakeConnections):
    def __init__(self):
        super().__init__()
        self.actor = 2
        self.owners = []

    def authorize(self, owner):
        self.owners.append(owner)
        return {"actingUserId": self.actor}


def test_actor_binding_survives_restart_and_prevents_identity_switch(tmp_path):
    store, settings, _ = prepare(tmp_path)
    chat = create(store, owner=1)
    connection = ServiceConnections()
    connection.definitions = {
        "write": {"server": "esketit", "name": "submit_author", "schema": {}, "readonly": False}
    }
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoint.sqlite")) as saver:
        runtime = Runtime(store, connection, settings, saver, model=object())
        runtime.authorize(chat["id"])
        assert store.get(chat["id"])["owner"] == 1
        assert store.get(chat["id"])["actingUserId"] == 2
    recovered = Store(str(store.engine.url))
    connection.actor = 3
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoint.sqlite")) as saver:
        runtime = Runtime(recovered, connection, settings, saver, model=object())
        with pytest.raises(PermissionError):
            runtime.execute(
                chat["id"], {"name": "write", "call_id": "different-actor", "arguments": "{}"}
            )
    assert connection.calls == []
    assert connection.owners == [1, 1]


def test_shared_item_queue_crosses_owners_without_leaking_chat_ids(tmp_path):
    store, _, _ = prepare(tmp_path)
    first = create(store, owner=1)
    second = create(store, owner=2)
    unrelated = create(store, owner=2, item=9)
    assert store.claim(second["id"]) is None
    assert store.get(second["id"])["status"] == "waiting_dependency"
    assert store.get(second["id"])["blockedBy"] == []
    assert store.claim(unrelated["id"])
    store.control(1, first["id"], "cancel")
    assert store.claim(second["id"])


def test_dynamic_reservation_waits_for_another_owners_active_request(tmp_path):
    store, _, _ = prepare(tmp_path)
    earlier = create(store, owner=1, item=9)
    later = create(store, owner=2, item=1)
    assert store.claim(later["id"])
    assert store.reserve(earlier["id"], [{"type": "track", "id": 1}])
    assert store.get(earlier["id"])["blockedBy"] == []


def test_push_is_sent_to_chat_owner_not_mcp_actor(tmp_path, monkeypatch):
    store, _, _ = prepare(tmp_path)
    connection = ServiceConnections()
    chat = create(store, owner=1)
    store.question(chat["id"], "choice", "Which version?", ["Original"])
    with store.transaction() as session:
        session.add(Device(token="chat-owner-device", owner=1, locale="en"))
        session.add(Device(token="mcp-actor-device", owner=2, locale="en"))
    sent = []
    monkeypatch.setattr("esketit_agent.notifications.firebase_admin.get_app", lambda: object())
    monkeypatch.setattr("esketit_agent.notifications.messaging.send", sent.append)
    Notifications(store, connection).dispatch()
    assert [message.token for message in sent] == ["chat-owner-device"]
    assert connection.owners == [1]
    with store.transaction() as session:
        assert session.query(Notice).one().delivered


def test_legacy_chat_keeps_its_original_mcp_identity(tmp_path):
    store, settings, _ = prepare(tmp_path)
    chat = create(store, owner=2)
    with store.transaction() as session:
        row = store.find(session, chat["id"])
        data = dict(row.data)
        data.pop("actingUserId")
        store.save(row, data)
    connection = ServiceConnections()
    connection.actor = 3
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoint.sqlite")) as saver:
        runtime = Runtime(store, connection, settings, saver, model=object())
        with pytest.raises(PermissionError):
            runtime.authorize(chat["id"])


def test_upload_uses_service_credentials_after_chat_owner_authorization(tmp_path, monkeypatch):
    _, settings, _ = prepare(tmp_path)
    connection = Connections(replace(settings, media_root=str(tmp_path)))
    connection.call = lambda *args: {"data": {"actingUserId": 2, "uploads": {"maxAudioBytes": 32}}}
    authorized = []
    sent = []

    def access(url, **kwargs):
        authorized.append(url)
        return SimpleNamespace(status_code=200)

    def upload(url, **kwargs):
        sent.append(kwargs["headers"])
        return SimpleNamespace(
            raise_for_status=lambda: None, json=lambda: {"uploadToken": "uploaded"}
        )

    monkeypatch.setattr("esketit_agent.tools.httpx.get", access)
    monkeypatch.setattr("esketit_agent.tools.httpx.post", upload)
    audio = tmp_path / "test.mp3"
    audio.write_bytes(b"test audio")
    assert connection.upload({"path": str(audio), "kind": "audio"}, "receipt", 1) == {
        "uploadToken": "uploaded"
    }
    assert authorized == ["http://localhost/api/agent-internal/users/1"]
    assert sent[0]["Authorization"] == "Bearer " + settings.esketit_token


def test_mcp_actor_cannot_read_or_control_the_chat_owners_conversation(tmp_path):
    store, settings, _ = prepare(tmp_path)
    chat = create(store, owner=1)
    client = TestClient(create_app(settings, store=store, start_worker=False))
    headers = {"Authorization": "Bearer " + settings.service_token, "X-Esketit-User-Id": "2"}
    assert client.get("/internal/chats", headers=headers).json() == []
    assert client.get("/internal/chats/" + chat["id"], headers=headers).status_code == 404
    assert (
        client.post(
            "/internal/chats/" + chat["id"] + "/actions/cancel", headers=headers
        ).status_code
        == 404
    )
    headers["X-Esketit-User-Id"] = "1"
    assert client.get("/internal/chats/" + chat["id"], headers=headers).status_code == 200
