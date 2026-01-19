import json

from fastapi import APIRouter
from fastapi.responses import StreamingResponse
from langchain_core.messages import HumanMessage
from pydantic import BaseModel
from src.agent import build_graph

router = APIRouter()


class ChatRequest(BaseModel):
    prompt: str
    user_id: str


@router.post("/send")
async def send_message(request: ChatRequest):
    app = await build_graph()
    initial_state = {
        "messages": [HumanMessage(content=request.prompt)],
        "retry_count": 0,
        "errors": [],
        "user_id": request.user_id,
    }

    async def event_generator():
        async for event in app.astream_events(initial_state, version="v1"):
            event_type = event["event"]
            data = event["data"]

            if event_type == "on_tool_start":
                yield f"data: {json.dumps({'type': 'thinking', 'content': f'Accessing Tool: {event['name']}...'})}\n\n"

            elif event_type == "on_tool_end":
                raw_output = data.get("output", "")

                if hasattr(raw_output, "content"):
                    output = str(raw_output.content)
                else:
                    output = str(raw_output)

                if "Error" in output or "DiscordAPIError" in output:
                    yield f"data: {json.dumps({'type': 'error', 'content': f'Tool Failed: {output[:150]}...'})}\n\n"
                else:
                    yield f"data: {json.dumps({'type': 'thinking', 'content': f'Tool Result: {output[:150]}...'})}\n\n"

            elif event_type == "on_chat_model_stream":
                chunk = data["chunk"]
                if chunk.content:
                    payload = {"type": "token", "content": chunk.content}
                    yield f"data: {json.dumps(payload)}\n\n"

        yield "data: [DONE]\n\n"

    return StreamingResponse(event_generator(), media_type="text/event-stream")
