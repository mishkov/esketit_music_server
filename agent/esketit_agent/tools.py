import asyncio
import hashlib
import json
import re
from contextlib import AsyncExitStack
from pathlib import Path

import httpx
from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client
from mcp.client.streamable_http import streamablehttp_client


class ToolError(Exception):
    pass


def function_name(server, tool):
    original = server + "__" + tool
    normalized = re.sub(r"[^a-zA-Z0-9_-]", "_", original)
    if normalized != original or len(normalized) > 64:
        normalized = normalized[:40] + "_" + hashlib.sha256(original.encode()).hexdigest()[:20]
    return normalized


class Connections:
    def __init__(self, settings):
        self.settings = settings
        self.servers = dict(settings.connections)
        self.servers["esketit"] = {
            "url": settings.esketit_url + "/mcp",
            "headers": {"Authorization": "Bearer " + settings.esketit_token},
        }
        self.definitions = {}

    async def _session(self, stack, server):
        configuration = self.servers[server]
        if "command" in configuration:
            transport = await stack.enter_async_context(
                stdio_client(
                    StdioServerParameters(
                        command=configuration["command"],
                        args=configuration.get("args", []),
                        env=configuration.get("env"),
                    )
                )
            )
        else:
            transport = await stack.enter_async_context(
                streamablehttp_client(
                    configuration["url"], headers=configuration.get("headers", {})
                )
            )
        session = await stack.enter_async_context(ClientSession(transport[0], transport[1]))
        await session.initialize()
        return session

    async def _discover(self):
        definitions = {}
        for server in self.servers:
            async with AsyncExitStack() as stack:
                session = await self._session(stack, server)
                cursor = None
                while True:
                    page = await session.list_tools(cursor=cursor)
                    for tool in page.tools:
                        name = function_name(server, tool.name)
                        definitions[name] = {
                            "server": server,
                            "name": tool.name,
                            "schema": tool.inputSchema,
                            "description": tool.description or tool.name,
                            "readonly": bool(tool.annotations and tool.annotations.readOnlyHint),
                        }
                    cursor = page.nextCursor
                    if not cursor:
                        break
        self.definitions = definitions

    def discover(self):
        asyncio.run(self._discover())
        return [
            {
                "type": "function",
                "name": name,
                "description": value["description"],
                "parameters": value["schema"],
                "strict": False,
            }
            for name, value in self.definitions.items()
        ]

    async def _call(self, server, name, arguments):
        async with AsyncExitStack() as stack:
            session = await self._session(stack, server)
            result = await session.call_tool(name, arguments)
            value = getattr(result, "structuredContent", None)
            if value is None:
                texts = [part.text for part in result.content if part.type == "text"]
                try:
                    value = json.loads("\n".join(texts))
                except (ValueError, TypeError):
                    value = {"text": "\n".join(texts)}
            if result.isError or value.get("ok") is False:
                raise ToolError(json.dumps(value, ensure_ascii=False))
            return value

    def call(self, server, name, arguments):
        return asyncio.run(self._call(server, name, arguments))

    def context(self):
        value = self.call("esketit", "get_context", {}).get("data", {})
        if type(value.get("actingUserId")) is not int or value["actingUserId"] <= 0:
            raise PermissionError("MCP acting account is not configured")
        return value

    def authorize(self, owner):
        response = httpx.get(
            self.settings.esketit_url + f"/api/agent-internal/users/{owner}",
            headers={"Authorization": "Bearer " + self.settings.service_token},
            timeout=20,
        )
        if response.status_code != 200:
            raise PermissionError("Agent permission is no longer available")
        return self.context()

    def upload(self, arguments, operation_id, owner):
        context = self.authorize(owner)
        root = Path(self.settings.media_root).resolve()
        path = Path(arguments["path"]).resolve(strict=True)
        if not path.is_relative_to(root) or not path.is_file():
            raise ValueError("Media must be a file inside AGENT_MEDIA_ROOT")
        kind = arguments["kind"]
        if kind not in ("audio", "author_photo", "album_cover"):
            raise ValueError("Invalid media kind")
        limit = context["uploads"]["maxAudioBytes" if kind == "audio" else "maxImageBytes"]
        if path.stat().st_size > limit:
            raise ValueError("Media exceeds the server upload limit")
        with path.open("rb") as stream:
            response = httpx.post(
                self.settings.esketit_url + "/api/mcp/uploads",
                headers={
                    "Authorization": "Bearer " + self.settings.esketit_token,
                    "Idempotency-Key": operation_id,
                },
                data={"kind": kind},
                files={"file": (path.name, stream)},
                timeout=360,
            )
        response.raise_for_status()
        return response.json()


ASK_USER = {
    "type": "function",
    "name": "ask_user",
    "description": "Ask a necessary question and wait for the user. Always allow a free-text answer.",
    "strict": True,
    "parameters": {
        "type": "object",
        "properties": {
            "question": {"type": "string"},
            "options": {"type": "array", "items": {"type": "string"}},
        },
        "required": ["question", "options"],
        "additionalProperties": False,
    },
}
FINISH_REQUEST = {
    "type": "function",
    "name": "finish_request",
    "description": (
        "Send the final answer when no question or tool action remains. "
        "This ends the current agent turn or waits for submitted reviews. "
        "Never put a question, choices, or an offer to continue in this message; "
        "use ask_user for any question. Never finish while review corrections remain."
    ),
    "strict": True,
    "parameters": {
        "type": "object",
        "properties": {"message": {"type": "string"}},
        "required": ["message"],
        "additionalProperties": False,
    },
}
UPLOAD_MEDIA = {
    "type": "function",
    "name": "upload_media",
    "description": "Upload a downloaded media file from the shared media directory to Esketit. Returns an upload token for MCP submissions.",
    "strict": True,
    "parameters": {
        "type": "object",
        "properties": {
            "path": {"type": "string"},
            "kind": {"type": "string", "enum": ["audio", "author_photo", "album_cover"]},
        },
        "required": ["path", "kind"],
        "additionalProperties": False,
    },
}


SEARCH_WEB = {
    "type": "function",
    "name": "search_web",
    "description": (
        "Search the live public internet and read relevant sources for music facts, "
        "release details, artists, versions or other public information. "
        "Returns a cited answer and source URLs. This does not download audio. "
        "Use only a public-information query, without secrets or private catalog notes."
    ),
    "strict": True,
    "parameters": {
        "type": "object",
        "properties": {"query": {"type": "string"}},
        "required": ["query"],
        "additionalProperties": False,
    },
}
