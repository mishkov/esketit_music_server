import copy
import uuid
from types import SimpleNamespace

import pytest
from langgraph.checkpoint.sqlite import SqliteSaver
from test_conversations import FakeConnections, create

from esketit_agent.notifications import Notifications
from esketit_agent.runtime import Runtime
from esketit_agent.settings import Settings
from esketit_agent.store import Notice, Receipt, Store
from esketit_agent.tools import ToolError, function_name


class ReviewConnections(FakeConnections):
    def __init__(self):
        super().__init__()
        self.review_status = "changes_requested"

    def call(self, server, name, arguments):
        if name == "get_submission":
            return {
                "data": {
                    "submissionId": 42,
                    "revision": 2,
                    "status": self.review_status,
                    "feedback": [{"comment": "Correct the language"}],
                }
            }
        return super().call(server, name, arguments)


def prepare(tmp_path, connection=None):
    store = Store("sqlite:///" + str(tmp_path / "store.sqlite"))
    settings = Settings("sqlite://", "x" * 32, "http://localhost", "m" * 32, "test")
    return store, settings, connection or FakeConnections()


def test_mcp_tool_names_are_valid_and_do_not_collide_after_normalization():
    assert function_name("esketit", "get_context") == "esketit__get_context"
    assert function_name("music", "track.search") != function_name("music", "track_search")
    assert len(function_name("music", "search" * 100)) <= 64
    assert "." not in function_name("music", "track.search")


def test_notification_without_recipients_does_not_replay_on_future_registration(tmp_path):
    store, _, connection = prepare(tmp_path)
    chat = create(store)
    store.question(chat["id"], "choice", "Which version?", ["Original"])
    Notifications(store, connection).dispatch()
    with store.transaction() as session:
        assert session.query(Notice).one().delivered


def test_review_feedback_does_not_restart_a_paused_request(tmp_path):
    store, settings, connection = prepare(tmp_path, ReviewConnections())
    chat = create(store)
    with store.transaction() as session:
        row = store.find(session, chat["id"])
        data = copy.deepcopy(row.data)
        data.update(
            status="waiting_review",
            submissions=[{"submissionId": 42, "revision": 1, "status": "pending_review"}],
        )
        store.save(row, data)
    store.control(2, chat["id"], "stop")
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoints.sqlite")) as saver:
        runtime = Runtime(store, connection, settings, saver, model=object())
        runtime.review(chat["id"])
    assert store.get(chat["id"])["status"] == "paused"
    assert store.get(chat["id"])["reviewNeedsWork"]
    store.control(2, chat["id"], "resume")
    assert store.get(chat["id"])["status"] == "queued"


def test_final_review_approval_finishes_the_chat_and_sends_completion_notice(tmp_path):
    connection = ReviewConnections()
    connection.review_status = "approved"
    store, settings, _ = prepare(tmp_path)
    chat = create(store)
    with store.transaction() as session:
        row = store.find(session, chat["id"])
        data = copy.deepcopy(row.data)
        data.update(
            status="waiting_review",
            submissions=[{"submissionId": 42, "revision": 1, "status": "pending_review"}],
        )
        store.save(row, data)
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoint.sqlite")) as saver:
        Runtime(store, connection, settings, saver, model=object()).review(chat["id"])
    assert store.get(chat["id"])["status"] == "completed"
    with store.transaction() as session:
        assert session.query(Notice).one().kind == "complete"


def test_followup_in_old_chat_keeps_arrival_order(tmp_path):
    store, _, _ = prepare(tmp_path)
    first = create(store)
    with store.transaction() as session:
        row = store.find(session, first["id"])
        data = copy.deepcopy(row.data)
        data["status"] = "completed"
        store.save(row, data)
    second = create(store)
    store.message(2, first["id"], str(uuid.uuid4()), "Now update the cover")
    assert store.claim(first["id"]) is None
    assert store.get(first["id"])["blockedBy"] == [second["id"]]


def test_retry_after_timeout_uses_exact_same_write_arguments(tmp_path):
    store, settings, connection = prepare(tmp_path)
    connection.definitions = {
        "esketit__submit_author": {
            "server": "esketit",
            "name": "submit_author",
            "schema": {"properties": {"requestId": {}}},
            "readonly": False,
        }
    }
    chat = create(store)
    attempts = []

    def write(server, name, arguments):
        attempts.append(copy.deepcopy(arguments))
        if len(attempts) == 1:
            raise TimeoutError("Response lost after server accepted the operation")
        return {
            "data": {
                "submissionId": 42,
                "entityType": "author",
                "entityId": 9,
                "status": "pending_review",
                "revision": 1,
            }
        }

    connection.call = write
    call = {
        "name": "esketit__submit_author",
        "call_id": "write-1",
        "arguments": '{"requestId":"model-generated","currentName":"Example Artist"}',
    }
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoint.sqlite")) as saver:
        runtime = Runtime(store, connection, settings, saver, model=object())
        with pytest.raises(TimeoutError):
            runtime.execute(chat["id"], call)
        runtime.execute(chat["id"], call)
        runtime.execute(chat["id"], call)
    assert len(attempts) == 2
    assert attempts[0] == attempts[1]
    assert attempts[0]["requestId"] != "model-generated"
    assert store.get(chat["id"])["submissions"][0]["submissionId"] == 42


def test_cached_write_recovers_submission_tracking_after_a_crash(tmp_path):
    store, settings, connection = prepare(tmp_path)
    chat = create(store)
    connection.definitions = {
        "write": {
            "server": "esketit",
            "name": "submit_author",
            "schema": {"properties": {"requestId": {}}},
            "readonly": False,
        }
    }
    with store.transaction() as session:
        session.add(
            Receipt(
                id=chat["id"] + ":write-1",
                chat_id=chat["id"],
                tool="write",
                arguments={},
                result='{"data":{"submissionId":42,"entityId":9,"entityType":"author","status":"pending_review"}}',
            )
        )
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoint.sqlite")) as saver:
        runtime = Runtime(store, connection, settings, saver, model=object())
        runtime.execute(chat["id"], {"name": "write", "call_id": "write-1", "arguments": "{}"})
    assert connection.calls == []
    assert store.get(chat["id"])["submissions"][0]["submissionId"] == 42


def test_missing_reference_is_reassessed_instead_of_pausing(tmp_path):
    store, settings, connection = prepare(tmp_path)
    chat = create(store)
    requests = []

    def missing(server, name, arguments):
        raise ToolError("The referenced track no longer exists")

    def respond(**arguments):
        requests.append(arguments)
        return SimpleNamespace(
            id="clarification", output=[], output_text="Which track did you mean?"
        )

    connection.call = missing
    model = SimpleNamespace(responses=SimpleNamespace(create=respond))
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoint.sqlite")) as saver:
        Runtime(store, connection, settings, saver, model=model).step({"chat_id": chat["id"]})
    assert "no longer exists" in requests[0]["instructions"]
    assert store.get(chat["id"])["status"] != "paused"


def test_review_correction_requires_a_tool_action_instead_of_a_final_offer(tmp_path):
    store, settings, connection = prepare(tmp_path)
    chat = create(store)
    with store.transaction() as session:
        row = store.find(session, chat["id"])
        data = copy.deepcopy(row.data)
        data["reviewNeedsWork"] = True
        store.save(row, data)
    requests = []

    def respond(**arguments):
        requests.append(arguments)
        return SimpleNamespace(id="unfinished", output=[], output_text="I can do that if you want.")

    model = SimpleNamespace(responses=SimpleNamespace(create=respond))
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoint.sqlite")) as saver:
        Runtime(store, connection, settings, saver, model=model).step({"chat_id": chat["id"]})
    assert requests[0]["tool_choice"] == "required"
    assert store.get(chat["id"])["status"] == "paused"


def test_waiting_tool_is_reassessed_after_predecessor_resolves(tmp_path):
    store, settings, connection = prepare(tmp_path)
    earlier = create(store, item=9)
    later = create(store, item=1)
    connection.definitions = {
        "write": {
            "server": "esketit",
            "name": "submit_track_update",
            "schema": {"properties": {"requestId": {}}},
            "readonly": False,
        }
    }
    configuration = {"configurable": {"thread_id": later["id"]}}
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoint.sqlite")) as saver:
        runtime = Runtime(store, connection, settings, saver, model=object())
        runtime.graph.invoke(
            {
                "chat_id": later["id"],
                "pending": [{"name": "write", "call_id": "stale", "arguments": '{"trackId":9}'}],
            },
            configuration,
        )
        assert runtime.snapshot(later["id"]).interrupts
        store.control(2, earlier["id"], "cancel")
        runtime.tick()
        result = runtime.snapshot(later["id"]).values
    assert not any(name == "submit_track_update" for name, _ in connection.calls)
    assert "reassess" in result["history"][-1]["output"]
    assert result["pending"] == []
