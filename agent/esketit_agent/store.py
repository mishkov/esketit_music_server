import copy
import hashlib
import re
import time
import uuid
from contextlib import contextmanager

from sqlalchemy import JSON, Integer, String, Text, create_engine, select, update
from sqlalchemy.orm import DeclarativeBase, Mapped, Session, mapped_column


class Base(DeclarativeBase):
    pass


class Mutex(Base):
    __tablename__ = "agent_mutex"
    id: Mapped[int] = mapped_column(primary_key=True)
    value: Mapped[int] = mapped_column(default=0)


class Chat(Base):
    __tablename__ = "agent_chats"
    sequence: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    id: Mapped[str] = mapped_column(String(36), unique=True)
    owner: Mapped[int] = mapped_column(index=True)
    data: Mapped[dict] = mapped_column(JSON)


class Receipt(Base):
    __tablename__ = "agent_tool_receipts"
    id: Mapped[str] = mapped_column(String(100), primary_key=True)
    chat_id: Mapped[str] = mapped_column(String(36), index=True)
    tool: Mapped[str] = mapped_column(String(100))
    arguments: Mapped[dict] = mapped_column(JSON)
    result: Mapped[str | None] = mapped_column(Text, nullable=True)


class Device(Base):
    __tablename__ = "agent_devices"
    token: Mapped[str] = mapped_column(String(4096), primary_key=True)
    owner: Mapped[int] = mapped_column(index=True)
    locale: Mapped[str] = mapped_column(String(10), default="en")


class Notice(Base):
    __tablename__ = "agent_notices"
    id: Mapped[str] = mapped_column(String(200), primary_key=True)
    owner: Mapped[int]
    chat_id: Mapped[str] = mapped_column(String(36))
    kind: Mapped[str] = mapped_column(String(30))
    next_attempt: Mapped[int] = mapped_column(default=0)
    attempts: Mapped[int] = mapped_column(default=0)
    delivered: Mapped[bool] = mapped_column(default=False)


def mentions(text):
    return [
        {"type": kind, "id": int(identifier), "label": ""}
        for kind, identifier in re.findall(r"@(track|album|author)-(\d+)\b", text)
        if int(identifier) > 0
    ]


def resource_key(item):
    return f"{item['type']}:{item['id']}"


def unresolved(data):
    return any(
        item["status"] in ("pending_review", "changes_requested")
        for item in data.get("submissions", [])
    )


def holds_place(data):
    return (
        (bool(data.get("lease")) and data.get("leaseUntil", 0) > time.time())
        or data["status"] not in ("completed", "cancelled")
        or unresolved(data)
    )


class Store:
    def __init__(self, url):
        self.engine = create_engine(url, pool_pre_ping=True)
        Base.metadata.create_all(self.engine)
        with Session(self.engine) as session, session.begin():
            if session.get(Mutex, 1) is None:
                session.add(Mutex(id=1))

    @contextmanager
    def transaction(self):
        with Session(self.engine, expire_on_commit=False) as session, session.begin():
            # Serialize queue decisions, including multi-item reservations, across processes.
            session.execute(update(Mutex).where(Mutex.id == 1).values(value=Mutex.value + 1))
            yield session

    def rows(self, session, owner=None):
        query = select(Chat).order_by(Chat.sequence)
        if owner is not None:
            query = query.where(Chat.owner == owner)
        return list(session.scalars(query))

    def find(self, session, identifier, owner=None):
        query = select(Chat).where(Chat.id == identifier)
        if owner is not None:
            query = query.where(Chat.owner == owner)
        row = session.scalar(query)
        if row is None:
            raise LookupError("Conversation not found")
        return row

    def save(self, row, data):
        data["updatedAt"] = int(time.time())
        row.data = copy.deepcopy(data)

    def get(self, identifier, owner=None):
        with Session(self.engine) as session:
            return copy.deepcopy(self.find(session, identifier, owner).data)

    def list(self, owner):
        with Session(self.engine) as session:
            return [copy.deepcopy(row.data) for row in reversed(self.rows(session, owner))]

    def create(self, owner, identifier, text, attachments, locale):
        attachments = list(
            {resource_key(item): item for item in attachments + mentions(text)}.values()
        )
        with self.transaction() as session:
            existing = session.scalar(select(Chat).where(Chat.id == identifier))
            if existing:
                if existing.owner != owner:
                    raise LookupError("Conversation not found")
                if (
                    existing.data["messages"][0]["text"] != text
                    or existing.data["attachments"] != attachments
                ):
                    raise ValueError("Request identifier already used with different content")
                return copy.deepcopy(existing.data)
            now = int(time.time())
            data = {
                "id": identifier,
                "title": text[:90],
                "status": "queued",
                "attachments": attachments,
                "resources": attachments,
                "messages": [{"id": identifier, "role": "user", "text": text}],
                "questions": [],
                "submissions": [],
                "updatedAt": now,
                "createdAt": now,
                "locale": locale,
                "owner": owner,
                "queueOrder": session.get(Mutex, 1).value,
                "instructionVersion": 1,
                "blockedBy": [],
                "resumeStatus": "queued",
                "lease": None,
                "leaseUntil": 0,
                "nextReview": 0,
            }
            session.add(Chat(id=identifier, owner=owner, data=data))
            return copy.deepcopy(data)

    def message(self, owner, identifier, message_id, text):
        if text.strip().casefold() in ("resume", "continue", "продолжить", "продолжай"):
            data = self.get(identifier, owner)
            if data["status"] == "paused":
                return self.control(owner, identifier, "resume")
        with self.transaction() as session:
            row = self.find(session, identifier, owner)
            data = copy.deepcopy(row.data)
            if any(item["id"] == message_id for item in data["messages"]):
                return data
            if data["status"] == "cancelled":
                raise ValueError("This request has been cancelled; start a new conversation")
            data["messages"].append({"id": message_id, "role": "user", "text": text})
            resources = {resource_key(item): item for item in data["resources"] + mentions(text)}
            data["resources"] = list(resources.values())
            data["instructionVersion"] += 1
            if data["status"] in ("completed", "failed"):
                data["status"] = "queued"
                data["queueOrder"] = session.get(Mutex, 1).value
            self.save(row, data)
            return data

    def control(self, owner, identifier, action):
        with self.transaction() as session:
            row = self.find(session, identifier, owner)
            data = copy.deepcopy(row.data)
            if action == "stop" and data["status"] not in ("paused", "completed", "cancelled"):
                data["resumeStatus"] = data["status"]
                data["status"] = "stopping" if data["lease"] else "paused"
            elif action == "resume" and data["status"] == "paused":
                data["status"] = data.get("resumeStatus", "queued")
                if data["status"] in ("running", "stopping", "failed"):
                    data["status"] = "queued"
                if data["status"] == "awaiting_answer" and all(
                    q.get("answer") is not None for q in data["questions"]
                ):
                    data["status"] = "queued"
                if data["status"] == "waiting_review":
                    if data.get("reviewNeedsWork"):
                        data["status"] = "queued"
                    elif not unresolved(data):
                        data["status"] = "completed"
            elif action == "cancel":
                data["status"] = "cancelled"
            self.save(row, data)
            return data

    def answer(self, owner, identifier, question_id, answer):
        with self.transaction() as session:
            row = self.find(session, identifier, owner)
            data = copy.deepcopy(row.data)
            question = next((q for q in data["questions"] if q["id"] == question_id), None)
            if question is None:
                raise LookupError("Question not found")
            if question.get("answer") is not None:
                if question["answer"] != answer:
                    raise ValueError("Question already answered")
                return data
            if data["status"] == "cancelled":
                raise ValueError("Request cancelled")
            question["answer"] = answer
            data["messages"].append({"id": question_id + ":answer", "role": "user", "text": answer})
            if data["status"] == "awaiting_answer":
                data["status"] = "queued"
            self.save(row, data)
            return data

    def notify(self, session, data, kind, key):
        identifier = f"{data['id']}:{hashlib.sha256(key.encode()).hexdigest()}"
        if session.get(Notice, identifier) is None:
            session.add(Notice(id=identifier, owner=data["owner"], chat_id=data["id"], kind=kind))

    def question(self, identifier, question_id, text, options):
        with self.transaction() as session:
            row = self.find(session, identifier)
            data = copy.deepcopy(row.data)
            question = next((q for q in data["questions"] if q["id"] == question_id), None)
            if question is None:
                question = {"id": question_id, "text": text, "options": options, "answer": None}
                data["questions"].append(question)
                data["messages"].append({"id": question_id, "role": "assistant", "text": text})
                self.notify(session, data, "question", question_id)
            if question.get("answer") is None and data["status"] not in (
                "paused",
                "stopping",
                "cancelled",
            ):
                data["status"] = "awaiting_answer"
            self.save(row, data)
            return question

    def blockers(self, session, row, resources):
        keys = {resource_key(item) for item in resources}
        return [
            other.id
            for other in self.rows(session, row.owner)
            if other.data.get("queueOrder", other.sequence)
            < row.data.get("queueOrder", row.sequence)
            and holds_place(other.data)
            and keys.intersection(resource_key(item) for item in other.data["resources"])
        ]

    def reserve(self, identifier, resources):
        with self.transaction() as session:
            row = self.find(session, identifier)
            data = copy.deepcopy(row.data)
            merged = {resource_key(item): item for item in data["resources"] + resources}
            data["resources"] = list(merged.values())
            blockers = self.blockers(session, row, data["resources"])
            # An earlier request discovering a new item must also wait for a later
            # request already changing it. Never overwrite in-flight/reviewed work.
            for other in self.rows(session, row.owner):
                if other.id == row.id or not holds_place(other.data):
                    continue
                if (
                    (
                        other.data["status"] not in ("queued", "waiting_dependency")
                        or unresolved(other.data)
                    )
                    and set(merged).intersection(
                        resource_key(item) for item in other.data["resources"]
                    )
                    and other.id not in blockers
                ):
                    blockers.append(other.id)
            data["blockedBy"] = blockers
            if blockers and data["status"] not in ("stopping", "paused", "cancelled"):
                data["status"] = "waiting_dependency"
            self.save(row, data)
            return blockers

    def claim(self, identifier):
        with self.transaction() as session:
            row = self.find(session, identifier)
            data = copy.deepcopy(row.data)
            now = int(time.time())
            if data["status"] == "waiting_dependency" and data.get("nextDependencyCheck", 0) > now:
                return None
            if data["status"] == "stopping" and data["leaseUntil"] < now:
                data["status"] = "paused"
                data["lease"] = None
                self.save(row, data)
            if data["status"] not in ("queued", "running", "waiting_dependency"):
                return None
            if data["leaseUntil"] > now:
                return None
            blockers = self.blockers(session, row, data["resources"])
            if blockers:
                data["blockedBy"] = blockers
                data["status"] = "waiting_dependency"
                self.save(row, data)
                return None
            data["blockedBy"] = []
            data["status"] = "running"
            data["lease"] = str(uuid.uuid4())
            data["leaseUntil"] = now + 180
            self.save(row, data)
            return data["lease"]

    def finish_step(self, identifier, lease):
        with self.transaction() as session:
            row = self.find(session, identifier)
            data = copy.deepcopy(row.data)
            if data["lease"] != lease:
                return
            data["lease"] = None
            data["leaseUntil"] = 0
            if data["status"] == "stopping":
                data["status"] = "paused"
            self.save(row, data)

    def renew(self, identifier, lease):
        with self.transaction() as session:
            row = self.find(session, identifier)
            data = copy.deepcopy(row.data)
            if data["lease"] != lease:
                return False
            data["leaseUntil"] = int(time.time()) + 180
            self.save(row, data)
            return True
