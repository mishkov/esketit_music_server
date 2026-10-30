"""Local OpenAI-protocol fixture for full-app tests without API spending.

Run this module on localhost and point OPENAI_BASE_URL to /v1, using a dummy
OPENAI_API_KEY. This supplies model responses; it never replaces app screens,
catalog APIs, MCP tools, persistence or the production entry point.
"""

import json
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        history = request.get("input", [])
        tool_output = next(
            (item for item in reversed(history) if item.get("type") == "function_call_output"), None
        )
        if tool_output is None:
            output = [
                {
                    "type": "function_call",
                    "id": "fc_" + uuid.uuid4().hex,
                    "call_id": "choice-" + uuid.uuid4().hex,
                    "name": "ask_user",
                    "arguments": json.dumps(
                        {
                            "question": "Which version should I use?",
                            "options": ["Original", "Deluxe"],
                        }
                    ),
                    "status": "completed",
                }
            ]
        else:
            choice = json.loads(tool_output["output"]).get("answer", "Original")
            output = [
                {
                    "type": "message",
                    "id": "msg_" + uuid.uuid4().hex,
                    "role": "assistant",
                    "status": "completed",
                    "content": [
                        {
                            "type": "output_text",
                            "text": "You selected " + choice + ". No catalog changes were made.",
                            "annotations": [],
                        }
                    ],
                }
            ]
        response = {
            "id": "resp_" + uuid.uuid4().hex,
            "object": "response",
            "created_at": int(time.time()),
            "status": "completed",
            "model": request["model"],
            "output": output,
            "parallel_tool_calls": False,
            "tool_choice": "auto",
            "tools": [],
            "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
        }
        body = json.dumps(response).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *arguments):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 8314), Handler).serve_forever()
