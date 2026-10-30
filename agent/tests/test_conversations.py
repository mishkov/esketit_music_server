import copy
import uuid

import pytest
from fastapi.testclient import TestClient
from langgraph.checkpoint.sqlite import SqliteSaver

from esketit_agent.api import create_app
from esketit_agent.runtime import Runtime
from esketit_agent.settings import Settings
from esketit_agent.store import Notice, Store


@pytest.fixture
def store(tmp_path):
    return Store("sqlite:///" + str(tmp_path / "agent.sqlite"))


def create(store, text="Add lyrics", item=1, owner=2):
    return store.create(
        owner,
        str(uuid.uuid4()),
        text,
        [{"type": "track", "id": item, "label": "Example track"}],
        "en",
    )


def test_paused_head_blocks_same_item_but_not_other_items(store):
    first, second, unrelated = create(store), create(store), create(store, item=2)
    store.control(2, first["id"], "stop")
    assert store.claim(second["id"]) is None
    assert store.get(second["id"])["blockedBy"] == [first["id"]]
    assert store.claim(unrelated["id"])
    store.control(2, first["id"], "cancel")
    assert store.claim(second["id"])


def test_cancel_does_not_release_pending_review(store):
    first, second = create(store), create(store)
    with store.transaction() as session:
        row = store.find(session, first["id"])
        data = copy.deepcopy(row.data)
        data["submissions"] = [{"submissionId": 42, "status": "pending_review"}]
        store.save(row, data)
    store.control(2, first["id"], "cancel")
    assert store.claim(second["id"]) is None


def test_cancelled_request_releases_expired_worker_lease_after_crash(store):
    first, second = create(store), create(store)
    assert store.claim(first["id"])
    store.control(2, first["id"], "cancel")
    assert store.claim(second["id"]) is None
    with store.transaction() as session:
        row = store.find(session, first["id"])
        data = copy.deepcopy(row.data)
        data["leaseUntil"] = 1
        store.save(row, data)
    recovered = Store(str(store.engine.url))
    assert recovered.claim(second["id"])


def test_stop_and_resume_survive_restart(store):
    chat = create(store)
    lease = store.claim(chat["id"])
    assert store.control(2, chat["id"], "stop")["status"] == "stopping"
    store.finish_step(chat["id"], lease)
    recovered = Store(str(store.engine.url))
    assert recovered.get(chat["id"])["status"] == "paused"
    recovered.message(2, chat["id"], str(uuid.uuid4()), "resume")
    assert recovered.claim(chat["id"])


def test_question_answer_is_durable_idempotent_and_does_not_unpause(store):
    chat = create(store)
    store.question(chat["id"], "question-1", "Which version?", ["Original", "Deluxe"])
    store.control(2, chat["id"], "stop")
    recovered = Store(str(store.engine.url))
    recovered.answer(2, chat["id"], "question-1", "Original")
    recovered.answer(2, chat["id"], "question-1", "Original")
    assert recovered.get(chat["id"])["status"] == "paused"
    with pytest.raises(ValueError):
        recovered.answer(2, chat["id"], "question-1", "Deluxe")
    with store.transaction() as session:
        assert len(list(session.query(Notice))) == 1
    messages = recovered.get(chat["id"])["messages"]
    assert [message["text"] for message in messages[-2:]] == ["Which version?", "Original"]


def test_create_retry_does_not_enqueue_twice(store):
    chat = create(store)
    store.create(2, chat["id"], "Add lyrics", chat["attachments"], "en")
    assert len(store.list(2)) == 1
    with pytest.raises(ValueError):
        store.create(2, chat["id"], "Different instruction", chat["attachments"], "en")


def test_ownership_and_internal_authentication(store):
    settings = Settings("sqlite://", "x" * 32, "http://localhost", "m" * 32, "test")
    client = TestClient(create_app(settings, store=store, start_worker=False))
    chat = create(store)
    assert client.get("/internal/chats").status_code == 401
    headers = {"Authorization": "Bearer " + "x" * 32, "X-Esketit-User-Id": "3"}
    assert client.get("/internal/chats/" + chat["id"], headers=headers).status_code == 404
    assert (
        client.post(
            "/internal/chats/" + chat["id"] + "/actions/cancel", headers=headers
        ).status_code
        == 404
    )
    assert client.get("/internal/chats", headers=headers).json() == []
    headers["X-Esketit-User-Id"] = "2"
    public = client.get("/internal/chats/" + chat["id"], headers=headers).json()
    assert "lease" not in public and "owner" not in public


class FakeConnections:
    def __init__(self):
        self.definitions = {}
        self.calls = []

    def authorize(self, owner):
        return {"actingUserId": owner}

    def discover(self):
        return []

    def call(self, server, name, arguments):
        self.calls.append((name, arguments))
        return {"ok": True, "data": {"items": [], "totalItems": 0}}


def test_langgraph_question_resumes_after_process_restart(store, tmp_path):
    settings = Settings("sqlite://", "x" * 32, "http://localhost", "m" * 32, "test")
    chat = create(store)
    checkpoint_path = str(tmp_path / "checkpoints.sqlite")
    configuration = {"configurable": {"thread_id": chat["id"]}}
    state = {
        "chat_id": chat["id"],
        "history": [],
        "pending": [
            {
                "name": "ask_user",
                "call_id": "choice",
                "arguments": '{"question":"Which version?","options":["Original","Deluxe"]}',
            }
        ],
    }
    with SqliteSaver.from_conn_string(checkpoint_path) as saver:
        runtime = Runtime(store, FakeConnections(), settings, saver, model=object())
        runtime.graph.invoke(state, configuration)
        assert runtime.snapshot(chat["id"]).interrupts
    recovered = Store(str(store.engine.url))
    recovered.answer(2, chat["id"], "choice", "Original")
    with SqliteSaver.from_conn_string(checkpoint_path) as saver:
        runtime = Runtime(recovered, FakeConnections(), settings, saver, model=object())
        runtime.tick()
        snapshot = runtime.snapshot(chat["id"])
        assert snapshot.values["pending"] == []
        assert '"Original"' in snapshot.values["history"][-1]["output"]
        assert recovered.get(chat["id"])["status"] == "running"


def test_answered_question_can_resume_after_manual_stop(store):
    chat = create(store)
    store.question(chat["id"], "question", "Which?", ["Original"])
    store.control(2, chat["id"], "stop")
    store.answer(2, chat["id"], "question", "Original")
    store.control(2, chat["id"], "resume")
    assert store.claim(chat["id"])


def test_mentions_reserve_the_same_item_as_attached_references(store):
    first = store.create(2, str(uuid.uuid4()), "Fix @track-1", [], "en")
    second = create(store)
    assert first["resources"][0]["id"] == 1
    assert store.claim(second["id"]) is None


def test_steering_cancels_unexecuted_calls(store, tmp_path):
    settings = Settings("sqlite://", "x" * 32, "http://localhost", "m" * 32, "test")
    chat = create(store)
    store.message(2, chat["id"], str(uuid.uuid4()), "Use another version instead")
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoint.sqlite")) as saver:
        connections = FakeConnections()
        runtime = Runtime(store, connections, settings, saver, model=object())
        result = runtime.step(
            {
                "chat_id": chat["id"],
                "instruction_version": 1,
                "pending": [{"name": "write", "call_id": "old", "arguments": "{}"}],
                "history": [],
            }
        )
        assert result["pending"] == []
        assert connections.calls == []
