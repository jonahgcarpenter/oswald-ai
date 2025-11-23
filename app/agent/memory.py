import os
from datetime import datetime

import httpx
from langchain.tools import ToolRuntime
from langchain_core.tools import tool
from sqlalchemy.future import select
from utils.create_tables import User, UserMemory
from utils.db_connect import AsyncSessionLocal
from utils.logger import get_logger

log = get_logger(__name__)

OLLAMA_BASE_URL = os.getenv("OLLAMA_BASE_URL")
OLLAMA_EMBEDDING_MODEL = os.getenv("OLLAMA_EMBEDDING_MODEL")

if not OLLAMA_BASE_URL:
    log.error("OLLAMA_BASE_URL environment variable not set.")
if not OLLAMA_EMBEDDING_MODEL:
    log.error("OLLAMA_EMBEDDING_MODEL environment variable not set.")


async def _get_ollama_embedding(text_to_embed: str) -> list[float]:
    """
    Generates an embedding for a given text using the Ollama API.
    """
    if not OLLAMA_BASE_URL or not OLLAMA_EMBEDDING_MODEL:
        log.error("Ollama embedding service is not configured.")
        return []

    try:
        async with httpx.AsyncClient(base_url=OLLAMA_BASE_URL, timeout=60.0) as client:
            response = await client.post(
                "/api/embeddings",
                json={"model": OLLAMA_EMBEDDING_MODEL, "prompt": text_to_embed},
            )
            response.raise_for_status()
            return response.json().get("embedding", [])
    except Exception as e:
        log.error(f"Ollama embedding error: {e}", exc_info=True)
        return []


class MemoryService:
    """
    Handles adding and retrieving vector memories for users.
    """

    async def add_memory(self, user_id: str, text: str, metadata: dict = None):
        """
        Adds a new memory for a specific user.
        """
        log.info(f"Adding memory for user: {user_id}")

        embedding = await _get_ollama_embedding(text)
        if not embedding:
            log.error(f"Failed to generate embedding for memory: {text[:30]}...")
            return

        async with AsyncSessionLocal() as session:
            try:
                user_stmt = select(User).where(User.id == user_id)
                user_result = await session.execute(user_stmt)
                user = user_result.scalar_one_or_none()

                if not user:
                    log.info(f"User {user_id} not found, creating new user.")
                    user = User(id=user_id)
                    session.add(user)
                    await session.commit()
                    await session.refresh(user)

                new_memory = UserMemory(
                    user_id=user.id,
                    content=text,
                    embedding=embedding,
                    memory_metadata=metadata,
                )
                session.add(new_memory)
                await session.commit()
                log.info(f"Successfully added memory for user: {user_id}")

            except Exception as e:
                log.error(
                    f"Database error while adding memory for {user_id}: {e}",
                    exc_info=True,
                )

    async def search_memories(
        self, user_id: str, query_text: str, k: int = 5
    ) -> list[str]:
        """
        Finds the 'k' most relevant memories for a specific user.
        """
        log.debug(
            f"Searching memory for user {user_id} with query: {query_text[:30]}..."
        )

        query_vector = await _get_ollama_embedding(query_text)
        if not query_vector:
            log.error("Failed to generate embedding for search query.")
            return []

        async with AsyncSessionLocal() as session:
            try:
                user_stmt = select(User).where(User.id == user_id)
                user_result = await session.execute(user_stmt)
                if not user_result.scalar_one_or_none():
                    log.debug(
                        f"No user found with id {user_id}, cannot search memories."
                    )
                    return []

                stmt = (
                    select(UserMemory)
                    .where(UserMemory.user_id == user_id)
                    .order_by(UserMemory.embedding.l2_distance(query_vector))
                    .limit(k)
                )

                result = await session.execute(stmt)
                memories = result.scalars().all()

                if not memories:
                    log.debug(f"No memories found for user {user_id}")
                    return []

                log.info(f"Found {len(memories)} relevant memories for user {user_id}")

                for mem in memories:
                    mem.last_accessed_at = datetime.utcnow()
                await session.commit()

                return [mem.content for mem in memories]

            except Exception as e:
                log.error(
                    f"Database error while searching memory for {user_id}: {e}",
                    exc_info=True,
                )
                return []


@tool
async def save_to_user_memory(text_to_remember: str, runtime: ToolRuntime) -> str:
    """
    Saves a persistent fact about the USER (e.g., name, hobbies, preferences).

    RULES:
    1. NEVER use this tool to save definitions, general knowledge, or your own answers.
    2. ONLY use this when the user explicitly tells you a fact about themselves.
    3. Reword the content to be a clear, third-person statement.
    """
    log.debug("Using save_to_user_memory tool")
    try:
        user_id = runtime.context["user_id"]
        memory_service = runtime.context["memory_service"]

        await memory_service.add_memory(user_id, text_to_remember)
        return f"Successfully saved: '{text_to_remember}' to memory."
    except KeyError:
        log.error(
            "Tool 'save_to_user_memory' called without user_id or memory_service in context."
        )
        return "Memory tool is not configured."
    except Exception as e:
        log.error(f"Error in save_to_user_memory: {e}", exc_info=True)
        return "An error occurred while saving to memory."


@tool
async def search_user_memory(query: str, runtime: ToolRuntime) -> str:
    """
    Searches your long-term memory for facts or context about the user.
    Use this tool when the user asks a question about themselves,
    their preferences, or past conversations.
    """
    log.debug("Using search_user_memory tool")
    try:
        user_id = runtime.context["user_id"]
        memory_service = runtime.context["memory_service"]

        results = await memory_service.search_memories(user_id, query)

        if not results:
            return "No relevant information found in memory."

        return "Found the following relevant information:\n" + "\n".join(results)
    except KeyError:
        log.error(
            "Tool 'search_user_memory' called without user_id or memory_service in context."
        )
        return "Memory tool is not configured."
    except Exception as e:
        log.error(f"Error in search_user_memory: {e}", exc_info=True)
        return "An error occurred while searching memory."
