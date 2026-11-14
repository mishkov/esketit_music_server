import hmac
import logging
import threading
from contextlib import asynccontextmanager, contextmanager
from typing import Literal
from uuid import UUID

from fastapi import Depends, FastAPI, Header, HTTPException, Response
from fastapi.responses import JSONResponse
from langgraph.checkpoint.postgres import PostgresSaver
from pydantic import BaseModel, Field

from .notifications import Notifications
from .runtime import Runtime
from .settings import Settings
from .store import Device, Store
from .tools import Connections

logger = logging.getLogger(__name__)


class Attachment(BaseModel):
    type: Literal["track", "album", "author"]
    id: int = Field(gt=0)
    label: str = Field(default="", max_length=300)


class CreateChat(BaseModel):
    id: UUID
    text: str = Field(min_length=1, max_length=20000)
    attachments: list[Attachment] = Field(default_factory=list, max_length=20)
    locale: Literal["en", "ru"] = "en"


class MessageInput(BaseModel):
    id: UUID
    text: str = Field(min_length=1, max_length=20000)


class AnswerInput(BaseModel):
    answer: str = Field(min_length=1, max_length=20000)


class DeviceInput(BaseModel):
    token: str = Field(min_length=1, max_length=4096)
    locale: Literal["en", "ru"] = "en"


def public_chat(data):
    # Never expose leases, model state, credentials or internal tool receipts.
    fields = (
        "id",
        "title",
        "status",
        "attachments",
        "messages",
        "questions",
        "submissions",
        "blockedBy",
        "updatedAt",
        "createdAt",
    )
    return {key: data[key] for key in fields}


@contextmanager
def persistent_graph(settings):
    connection = settings.database_url.replace("postgresql+psycopg://", "postgresql://")
    with PostgresSaver.from_conn_string(connection) as saver:
        saver.setup()
        yield saver


def create_app(settings, store=None, runtime=None, start_worker=True):
    store = store or Store(settings.database_url)
    stopping = threading.Event()

    def worker(runtime):
        notifications = Notifications(store, runtime.connections)
        while not stopping.is_set():
            try:
                runtime.tick()
                notifications.dispatch()
            except Exception:
                logger.exception("Agent scheduler iteration failed")
            stopping.wait(2)

    @asynccontextmanager
    async def lifespan(app):
        if not start_worker:
            yield
            return
        with persistent_graph(settings) as saver:
            active = runtime or Runtime(store, Connections(settings), settings, saver)
            thread = threading.Thread(target=worker, args=(active,), daemon=True)
            thread.start()
            yield
            stopping.set()
            thread.join(timeout=130)

    app = FastAPI(title="Esketit Agent", lifespan=lifespan, docs_url=None, redoc_url=None)

    def owner(authorization: str = Header(default=""), x_esketit_user_id: int = Header(default=0)):
        if not hmac.compare_digest(authorization, "Bearer " + settings.service_token):
            raise HTTPException(401)
        if x_esketit_user_id <= 0:
            raise HTTPException(401)
        return x_esketit_user_id

    @app.exception_handler(LookupError)
    async def missing(request, error):
        return JSONResponse(status_code=404, content={"error": "not_found"})

    @app.exception_handler(ValueError)
    async def conflict(request, error):
        return JSONResponse(status_code=409, content={"error": "conflict"})

    @app.get("/healthz")
    def health():
        return {"status": "ok"}

    @app.get("/internal/chats")
    def chats(user: int = Depends(owner)):
        return [public_chat(item) for item in store.list(user)]

    @app.post("/internal/chats")
    def create(body: CreateChat, user: int = Depends(owner)):
        if not body.text.strip():
            raise HTTPException(422)
        attachments = [item.model_dump() for item in body.attachments]
        return public_chat(
            store.create(user, str(body.id), body.text.strip(), attachments, body.locale)
        )

    @app.get("/internal/chats/{identifier}")
    def chat(identifier: UUID, user: int = Depends(owner)):
        return public_chat(store.get(str(identifier), user))

    @app.post("/internal/chats/{identifier}/messages")
    def message(identifier: UUID, body: MessageInput, user: int = Depends(owner)):
        if not body.text.strip():
            raise HTTPException(422)
        return public_chat(store.message(user, str(identifier), str(body.id), body.text.strip()))

    @app.post("/internal/chats/{identifier}/actions/{action}")
    def action(
        identifier: UUID, action: Literal["stop", "resume", "cancel"], user: int = Depends(owner)
    ):
        return public_chat(store.control(user, str(identifier), action))

    @app.post("/internal/chats/{identifier}/questions/{question}/answer")
    def answer(identifier: UUID, question: str, body: AnswerInput, user: int = Depends(owner)):
        if not body.answer.strip():
            raise HTTPException(422)
        return public_chat(store.answer(user, str(identifier), question, body.answer.strip()))

    @app.put("/internal/device")
    def register_device(body: DeviceInput, user: int = Depends(owner)):
        with store.transaction() as session:
            device = session.get(Device, body.token)
            if device:
                device.owner, device.locale = user, body.locale
            else:
                session.add(Device(token=body.token, owner=user, locale=body.locale))
        return {"registered": True}

    @app.delete("/internal/device")
    def remove_device(x_agent_device_token: str = Header(default=""), user: int = Depends(owner)):
        with store.transaction() as session:
            device = session.get(Device, x_agent_device_token)
            if device and device.owner == user:
                session.delete(device)
        return Response(status_code=204)

    return app


def application():
    return create_app(Settings.from_environment())
