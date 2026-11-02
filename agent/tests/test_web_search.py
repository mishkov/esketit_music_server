import json
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
from langgraph.checkpoint.sqlite import SqliteSaver
from test_conversations import FakeConnections, create

from esketit_agent.runtime import Runtime
from esketit_agent.settings import Settings
from esketit_agent.store import Store
from esketit_agent.tools import ToolError
from esketit_agent.web_search import WebSearch


def response(text="Released in 2010. [source]", annotations=None, sources=None, status="completed"):
    values = [
        {"type": "web_search_call", "status": status, "action": {"sources": sources or []}},
        {
            "type": "message",
            "content": [
                {
                    "type": "output_text",
                    "text": text,
                    "annotations": annotations or [],
                }
            ],
        },
    ]
    return SimpleNamespace(
        output=[SimpleNamespace(model_dump=lambda value=value, **_: value) for value in values]
    )


def client(result):
    return SimpleNamespace(responses=SimpleNamespace(create=Mock(return_value=result)))


def citation(url="https://label.example/release", title="Official label", start=18, end=26):
    return {
        "type": "url_citation",
        "url": url,
        "title": title,
        "start_index": start,
        "end_index": end,
    }


def test_live_search_uses_configured_model_and_only_public_query():
    model = client(response(annotations=[citation()]))
    result = WebSearch(model, "gpt-5.4-mini").search("  Nightcall release year  ")
    request = model.responses.create.call_args.kwargs
    assert request["model"] == "gpt-5.4-mini"
    assert request["input"] == "Nightcall release year"
    assert request["tools"] == [
        {"type": "web_search", "search_context_size": "low", "external_web_access": True}
    ]
    assert request["tool_choice"] == {"type": "web_search"}
    assert request["include"] == ["web_search_call.action.sources"]
    assert request["store"] is False
    assert result["answer"] == "Released in 2010. [Official label](<https://label.example/release>)"
    assert result["sources"] == [
        {"url": "https://label.example/release", "title": "Official label"}
    ]


@pytest.mark.parametrize("query", [None, 1, "", "   "])
def test_empty_queries_never_reach_remote_api(query):
    model = client(None)
    with pytest.raises(ToolError, match="nonempty"):
        WebSearch(model, "mini").search(query)
    model.responses.create.assert_not_called()


def test_multiple_citations_escape_titles_urls_and_remove_provider_markers():
    text = "2010. \ue200cite\ue202turn0search0\ue201"
    first = citation(title="Label [release]\npage", start=6, end=len(text))
    second = citation("https://artist.example/a b>c", "Artist", 6, len(text))
    result = WebSearch(client(response(text, [first, second, first])), "mini").search("release")
    assert result["answer"] == (
        "2010. [Label \\[release\\] page](<https://label.example/release>) "
        "[Artist](<https://artist.example/a%20b%3Ec>)"
    )
    assert len(result["sources"]) == 2
    assert "\ue200" not in result["answer"]


def test_missing_or_invalid_annotation_span_still_has_clickable_sources():
    result = WebSearch(
        client(
            response(
                "2010. \ue200cite\ue202turn0search0\ue201",
                [citation(start=-1)],
                sources=[{"url": "https://artist.example/release"}],
            )
        ),
        "mini",
    ).search("release")
    assert "\ue200" not in result["answer"]
    assert "[Official label](<https://label.example/release>)" in result["answer"]
    assert "[artist.example](<https://artist.example/release>)" in result["answer"]


@pytest.mark.parametrize(
    "url",
    [
        "javascript:alert(1)",
        "file:///secret",
        "https://user:pass@host/",
        "https://[invalid",
        "/relative",
        None,
    ],
)
def test_unusable_source_urls_are_not_returned(url):
    with pytest.raises(ToolError, match="unverified"):
        WebSearch(client(response(annotations=[citation(url=url)])), "mini").search("release")


@pytest.mark.parametrize(
    "result",
    [
        SimpleNamespace(output=[]),
        response(status="failed"),
        response(),
        response(text="", sources=[{"url": "https://example.com"}]),
    ],
)
def test_missing_or_failed_research_never_claims_verified_answer(result):
    with pytest.raises(ToolError, match="unverified"):
        WebSearch(client(result), "mini").search("release")


def test_search_result_receipt_survives_restart_without_another_paid_call(tmp_path):
    database = "sqlite:///" + str(tmp_path / "store.sqlite")
    store = Store(database)
    chat = create(store)
    settings = Settings("sqlite://", "x" * 32, "http://localhost", "m" * 32, "mini")
    model = client(response(annotations=[citation()]))
    call = {"name": "search_web", "call_id": "research", "arguments": '{"query":"release year"}'}
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoints.sqlite")) as saver:
        first = Runtime(store, FakeConnections(), settings, saver, model=model)
        result = first.execute(chat["id"], call)
        recovered = Runtime(Store(database), FakeConnections(), settings, saver, model=model)
        assert recovered.execute(chat["id"], call) == result
    assert "https://label.example/release" in json.loads(result)["answer"]
    assert model.responses.create.call_count == 1


def test_interrupted_readonly_search_can_retry_without_write_reconciliation(tmp_path):
    store = Store("sqlite:///" + str(tmp_path / "store.sqlite"))
    chat = create(store)
    settings = Settings("sqlite://", "x" * 32, "http://localhost", "m" * 32, "mini")
    model = client(None)
    model.responses.create.side_effect = [
        TimeoutError("lost response"),
        response(annotations=[citation()]),
    ]
    call = {"name": "search_web", "call_id": "research", "arguments": '{"query":"release year"}'}
    with SqliteSaver.from_conn_string(str(tmp_path / "checkpoints.sqlite")) as saver:
        runtime = Runtime(store, FakeConnections(), settings, saver, model=model)
        with pytest.raises(TimeoutError):
            runtime.execute(chat["id"], call)
        assert "answer" in json.loads(runtime.execute(chat["id"], call))
    assert store.get(chat["id"])["questions"] == []


def test_uncited_plain_url_answer_still_gets_explicit_source_links():
    url = "https://label.example/release"
    result = WebSearch(
        client(
            response(
                text="2010. Source: " + url,
                sources=[{"url": url, "title": "Label"}],
            )
        ),
        "mini",
    ).search("release year")
    assert "[Label](<" + url + ">)" in result["answer"]
