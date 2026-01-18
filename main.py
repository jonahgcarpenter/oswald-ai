from contextlib import asynccontextmanager

import uvicorn
from fastapi import APIRouter, FastAPI
from fastapi.middleware.cors import CORSMiddleware

from src.mcp_client import close_mcp_client, initialize_mcp_client
from src.routers import chat, system
from src.util.args import get_server_config
from src.util.vars import settings


@asynccontextmanager
async def lifespan(app: FastAPI):
    try:
        await initialize_mcp_client()
    except Exception as e:
        print(f"Startup Failed: {e}")

    yield

    await close_mcp_client()


app = FastAPI(title="Oswald Agent API", lifespan=lifespan)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

api = APIRouter()

api.include_router(system.router, prefix="/system", tags=["System"])
api.include_router(chat.router, prefix="/chat", tags=["Chat"])

app.include_router(api, prefix=f"/api/{settings.API_VERSION}")

if __name__ == "__main__":
    config = get_server_config()

    uvicorn.run("main:app", host=config.host, port=config.port, reload=config.reload)
