import os
from datetime import datetime

import httpx
from sqlalchemy.future import select
from utils.config import settings
from utils.create_tables import User, UserMemory
from utils.db_connect import AsyncSessionLocal
from utils.logger import get_logger

log = get_logger(__name__)

OLLAMA_BASE_URL = settings.OLLAMA_BASE_URL
OLLAMA_EMBEDDING_MODEL = settings.OLLAMA_EMBEDDING_MODEL


async def _get_ollama_embedding(text_to_embed: str) -> list[float]:
    """
    Generates an embedding for a given text using the Ollama API.
    """
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

    async def add_memory(self, user_id: str, text: str):
        """
        Adds a new memory for a specific user.
        """
        log.info(f"Adding memory for user")

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
                )
                session.add(new_memory)
                await session.commit()
                log.info(f"Successfully added memory for user: '{text}'")

            except Exception as e:
                log.error(
                    f"Database error while adding memory for: {e}",
                    exc_info=True,
                )

    async def search_memories(
        self, user_id: str, query_text: str, k: int = 5
    ) -> list[str]:
        """
        Finds the 'k' most relevant memories for a specific user.
        """
        log.debug(f"Searching memory for user with query: {query_text[:30]}...")

        query_vector = await _get_ollama_embedding(query_text)
        if not query_vector:
            log.error("Failed to generate embedding for search query.")
            return []

        async with AsyncSessionLocal() as session:
            try:
                user_stmt = select(User).where(User.id == user_id)
                user_result = await session.execute(user_stmt)
                if not user_result.scalar_one_or_none():
                    log.debug(f"No user found with this id, cannot search memories.")
                    return []

                stmt = (
                    select(
                        UserMemory,
                        UserMemory.embedding.l2_distance(query_vector).label(
                            "distance"
                        ),
                    )
                    .where(UserMemory.user_id == user_id)
                    .order_by("distance")
                    .limit(k)
                )

                result = await session.execute(stmt)
                rows = result.all()
                memories = [row[0] for row in rows if row[1] < 0.6]

                if not memories:
                    log.debug(f"No memories found for user")
                    return []

                log.info(f"Found {len(memories)} relevant memories for user")

                for mem in memories:
                    mem.last_accessed_at = datetime.utcnow()
                await session.commit()

                return [mem.content for mem in memories]

            except Exception as e:
                log.error(
                    f"Database error while searching memory for user: {e}",
                    exc_info=True,
                )
                return []
