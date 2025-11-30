import asyncio
import logging.config
import os
import subprocess
import sys
import warnings
from contextlib import asynccontextmanager

warnings.filterwarnings("ignore", message=".*Pydantic serializer warnings.*")

import uvicorn
from agent.agent import OllamaService
from agent.endpoints import router as agent_router
from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse
from sqlalchemy.ext.asyncio import AsyncSession
from utils.create_tables import create_db_and_tables
from utils.db_connect import engine, get_db_session, get_db_status
from utils.logger import LOGGING_CONFIG, get_logger

logging.config.dictConfig(LOGGING_CONFIG)

log = get_logger(__name__)


@asynccontextmanager
async def lifespan(app: FastAPI):
    """
    Manages the application's lifespan events.
    """
    log.info("FastAPI app starting up...")

    env = os.environ.copy()
    env["PYTHONPATH"] = os.getcwd()
    # discord_process = subprocess.Popen(
    #     [sys.executable, "integrations/discord_client.py"],
    #     stdout=sys.stdout,
    #     stderr=sys.stderr,
    #     env=env,
    # )
    # log.info(f"Discord bot started with PID: {discord_process.pid}")

    try:
        await create_db_and_tables(engine)
        log.info("Database tables initialized successfully.")
    except Exception as e:
        log.error(f"Failed to initialize database tables: {e}", exc_info=True)
    app.state.ollama_service = OllamaService()
    log.info("OllamaService loaded.")
    yield
    log.info("FastAPI app shutting down...")

    # if discord_process.poll() is None:
    #     log.info("Stopping Discord bot...")
    #     discord_process.terminate()
    #     try:
    #         discord_process.wait(timeout=5)
    #     except subprocess.TimeoutExpired:
    #         discord_process.kill()
    #     log.info("Discord bot stopped.")

    await engine.dispose()
    log.info("Database engine disposed.")


app = FastAPI(lifespan=lifespan)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


@app.get("/")
async def serve_test_ui():
    return FileResponse("views/ui.html")


@app.get("/health")
async def read_root_health_check(
    request: Request,
    db: AsyncSession = Depends(get_db_session),
):
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
    uvicorn.run(
        "main:app", host="0.0.0.0", port=8000, reload=True, log_config=LOGGING_CONFIG
    )
