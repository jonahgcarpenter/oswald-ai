from contextlib import asynccontextmanager

import uvicorn
from fastapi import Depends, FastAPI, HTTPException
from sqlalchemy.ext.asyncio import AsyncSession
from utils.db_connect import engine, get_db_session, get_db_status


@asynccontextmanager
async def lifespan(app: FastAPI):
    """
    Manages the application's lifespan events.
    - On startup: Prints a message.
    - On shutdown: Disposes of the database engine's connection pool.
    """
    print("INFO:     FastAPI app starting up...")

    yield

    print("INFO:     FastAPI app shutting down...")
    await engine.dispose()
    print("INFO:     Database engine disposed.")


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
        "main:app",
        host="127.0.0.1",
        port=8000,
        reload=True,
    )
