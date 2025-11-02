import asyncio
from contextlib import asynccontextmanager

import uvicorn
from agent.agent import OllamaService
from agent.endpoints import router as agent_router
from fastapi import Depends, FastAPI, HTTPException, Request
from sqlalchemy.ext.asyncio import AsyncSession
from utils.db_connect import engine, get_db_session, get_db_status
from utils.logger import LOGGING_CONFIG, get_logger

log = get_logger(__name__)


@asynccontextmanager
async def lifespan(app: FastAPI):
    """
    Manages the application's lifespan events.
    - On startup: Prints a message.
    - On shutdown: Disposes of the database engine's connection pool.
    """
    log.info("FastAPI app starting up...")
    app.state.ollama_service = OllamaService()
    log.info("OllamaService loaded.")
    yield
    log.info("FastAPI app shutting down...")
    await engine.dispose()
    log.info("Database engine disposed.")


app = FastAPI(lifespan=lifespan)


@app.get("/")
async def read_root_health_check(
    request: Request,
    db: AsyncSession = Depends(get_db_session),
):
    """
    Root endpoint health check.
    """
    ollama_service: OllamaService = request.app.state.ollama_service

    db_status_task = get_db_status(db)
    ollama_status_task = ollama_service.check_health()

    db_report, ollama_report = await asyncio.gather(db_status_task, ollama_status_task)

    combined_report = {"database": db_report, "ollama": ollama_report}

    if db_report["status"] == "ok" and ollama_report["status"] == "ok":
        return combined_report
    else:
        raise HTTPException(status_code=503, detail=combined_report)


app.include_router(agent_router, prefix="/agent")


if __name__ == "__main__":
    """
    Starts the Uvicorn server.
    """
    uvicorn.run(
        "main:app", host="127.0.0.1", port=8000, reload=True, log_config=LOGGING_CONFIG
    )
