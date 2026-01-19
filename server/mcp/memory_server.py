import logging
import os

import asyncpg
from langchain_ollama import OllamaEmbeddings
from mcp.server.fastmcp import FastMCP

DB_URL = os.environ["DATABASE_URL"]
OLLAMA_URL = os.environ["OLLAMA_URL"]
EMBEDDING_MODEL = os.environ["OLLAMA_EMBEDDING_MODEL"]
VECTOR_DIM = int(os.getenv("VECTOR_DIM", "768"))

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

mcp = FastMCP("memory")
embeddings = OllamaEmbeddings(base_url=OLLAMA_URL, model=EMBEDDING_MODEL)

_db_pool = None


async def get_db():
    global _db_pool
    if not _db_pool:
        _db_pool = await asyncpg.create_pool(DB_URL)
    return _db_pool


async def ensure_schema():
    pool = await get_db()
    async with pool.acquire() as conn:
        await conn.execute("CREATE EXTENSION IF NOT EXISTS vector;")

        await conn.execute(
            f"""
            CREATE TABLE IF NOT EXISTS memories (
                id SERIAL PRIMARY KEY,
                user_id TEXT NOT NULL, 
                content TEXT NOT NULL,
                category TEXT,
                embedding vector({VECTOR_DIM}),
                created_at TIMESTAMPTZ DEFAULT NOW()
            );
        """
        )

        await conn.execute(
            "CREATE INDEX IF NOT EXISTS memory_user_idx ON memories(user_id);"
        )


@mcp.tool()
async def save_memory(user_id: str, content: str, category: str = "general") -> str:
    """
    Save a persistent memory for a specific user.
    Args:
        user_id: The unique ID of the user.
        content: The fact to remember.
        category: The category (e.g., 'preferences', 'work').
    """
    try:
        await ensure_schema()

        logger.info(f"Saving for user {user_id}: {content[:30]}...")
        vector = await embeddings.aembed_query(content)

        pool = await get_db()
        async with pool.acquire() as conn:
            await conn.execute(
                """
                INSERT INTO memories (user_id, content, category, embedding) 
                VALUES ($1, $2, $3, $4)
                """,
                user_id,
                content,
                category,
                str(vector),
            )

        return f"Memory saved for User {user_id}."

    except Exception as e:
        logger.error(f"Error saving memory: {e}")
        return f"System Error: Failed to save memory. {str(e)}"


@mcp.tool()
async def search_memory(user_id: str, query: str, threshold: float = 0.6) -> str:
    """
    Search strictly within a specific user's memories.
    Args:
        user_id: The unique ID of the user (must match who is asking).
        query: The search text.
    """
    try:
        await ensure_schema()

        vector = await embeddings.aembed_query(query)

        pool = await get_db()
        rows = []
        async with pool.acquire() as conn:
            rows = await conn.fetch(
                """
                SELECT content, category, 1 - (embedding <=> $2) as similarity
                FROM memories
                WHERE user_id = $1 AND 1 - (embedding <=> $2) > $3
                ORDER BY similarity DESC
                LIMIT 5
                """,
                user_id,
                str(vector),
                threshold,
            )

        if not rows:
            return f"No relevant memories found for user {user_id}."

        results = [f"--- Memory Found (User: {user_id}) ---"]
        for r in rows:
            score = f"{r['similarity']:.2f}"
            results.append(f"[{score}] {r['content']}")

        return "\n".join(results)

    except Exception as e:
        logger.error(f"Error searching memory: {e}")
        return f"System Error: {str(e)}"


if __name__ == "__main__":
    mcp.run()
