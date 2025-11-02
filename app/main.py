from contextlib import asynccontextmanager

import uvicorn
from fastapi import Depends, FastAPI, HTTPException
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
    yield
    log.info("FastAPI app shutting down...")
    await engine.dispose()
    log.info("Database engine disposed.")


app = FastAPI(lifespan=lifespan)


@app.get("/")
async def read_root_health_check(
    db: AsyncSession = Depends(get_db_session),
):
    """
    Root endpoint health check.
    """
    status_report = await get_db_status(db)

    if status_report["status"] == "ok":
        return status_report
    else:
        raise HTTPException(status_code=503, detail=status_report)


if __name__ == "__main__":
    """
    Starts the Uvicorn server.
    """
    uvicorn.run(
        "main:app", host="127.0.0.1", port=8000, reload=True, log_config=LOGGING_CONFIG
    )
