import json
import os
from dataclasses import dataclass, field


@dataclass(frozen=True)
class Settings:
    database_url: str
    service_token: str
    esketit_url: str
    esketit_token: str
    model: str
    connections: dict = field(default_factory=dict)
    media_root: str = "/media"
    review_interval: int = 600

    @classmethod
    def from_environment(cls):
        token = os.environ["AGENT_SERVICE_TOKEN"]
        if len(token) < 32:
            raise ValueError("AGENT_SERVICE_TOKEN must contain at least 32 characters")
        connections = json.loads(os.environ.get("AGENT_MCP_SERVERS") or "{}")
        return cls(
            database_url=os.environ["AGENT_DATABASE_URL"],
            service_token=token,
            esketit_url=os.environ["ESKETIT_URL"].rstrip("/"),
            esketit_token=os.environ["ESKETIT_MCP_TOKEN"],
            model=os.environ.get("OPENAI_MODEL", "gpt-5.4-mini"),
            connections=connections,
            media_root=os.environ.get("AGENT_MEDIA_ROOT", "/media"),
        )
