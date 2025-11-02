from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel
from utils.logger import get_logger

from .agent import OllamaService

log = get_logger(__name__)

router = APIRouter()


class AgentRequest(BaseModel):
    question: str
    user_id: str


async def get_ollama_service(request: Request) -> OllamaService:
    """
    A dependency that retrieves the singleton OllamaService
    instance from the app's state.
    """
    return request.app.state.ollama_service


@router.post("/generate")
async def ask_agent(
    request: AgentRequest,
    ollama: OllamaService = Depends(get_ollama_service),
):
    """
    Receives a question and passes it to the OllamaService.
    """
    if not ollama:
        log.error("OllamaService not available.")
        raise HTTPException(
            status_code=500, detail="Ollama service is not initialized."
        )

    log.info(f"Received question for agent: {request.question}")

    response = await ollama.ask_question(
        question=request.question, user_id=request.user_id
    )

    return {"answer": response}
