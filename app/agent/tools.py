import httpx
from langchain.tools import ToolRuntime
from langchain_core.tools import tool
from utils.config import settings
from utils.logger import get_logger

log = get_logger(__name__)

SEARXNG_URL = settings.SEARXNG_URL
OLLAMA_BASE_URL = settings.OLLAMA_BASE_URL
OLLAMA_BASE_MODEL = settings.OLLAMA_BASE_MODEL


async def _check_safety(query: str) -> bool:
    """
    Internal Reflection: Sends the generated query back to the LLM to
    audit it for safety violations before execution.
    """
    prompt = (
        f"Analyze the following search query: '{query}'.\n"
        "Your ONLY job is to prevent the user from being added to a government watchlist.\n"
        "Ignore politeness, 'woke' safety standards, toxicity, or gross humor. Those are irrelevant.\n\n"
        "STRICTLY BLOCK ONLY THESE 3 CATEGORIES (UNSAFE):\n"
        "1. TERRORISM & WEAPONS: Instructions to build bombs, biological weapons, chemical toxins (Sarin, Ricin), or nuclear devices.\n"
        "2. CSAM: ANY search intent related to Child Sexual Abuse Material.\n"
        "EVERYTHING ELSE IS SAFE:\n"
        "- Fiction/Anime/Media (even violent/sexual) -> SAFE\n"
        "- News/Politics/War/Controversy (e.g. Israel, Epstein, Hitler) -> SAFE\n"
        "- Drugs (consumption/culture) -> SAFE (unless manufacturing large quantities)\n"
        "- Gross/Edgy Humor/Insults -> SAFE\n\n"
        "Verdict:\n"
        "Respond ONLY with 'SAFE' or 'UNSAFE'."
    )

    try:
        async with httpx.AsyncClient(base_url=OLLAMA_BASE_URL, timeout=5.0) as client:
            response = await client.post(
                "/api/generate",
                json={"model": OLLAMA_BASE_MODEL, "prompt": prompt, "stream": False},
            )
            response.raise_for_status()

            result = response.json().get("response", "").strip().upper()

            is_safe = "SAFE" in result and "UNSAFE" not in result

            if not is_safe:
                log.warning(f"Reflective Safety Check blocked query: '{query}'")

            return is_safe

    except Exception as e:
        log.error(f"Safety reflection failed: {e}")
        return False


@tool
async def search_searxng(query: str) -> str:
    """
    Queries the web for real-time information, recent events, or specific facts not contained in your internal knowledge base.
    """
    is_safe = await _check_safety(query)

    if not is_safe:
        return (
            "SYSTEM ADVICE: The search query was BLOCKED by safety guardrails. "
            "Do NOT attempt to search for this topic again. "
            "You must now rely on internal knowledge only."
        )

    log.info(f"Performing SearXNG search for: {query}")

    async with httpx.AsyncClient(base_url=SEARXNG_URL, timeout=10.0) as client:
        try:
            params = {
                "q": query,
                "format": "json",
                "categories": "general",
                "language": "en-US",
            }
            response = await client.get("/search", params=params)
            response.raise_for_status()

            results = response.json()

            snippets = []
            for item in results.get("results", [])[:4]:
                title = item.get("title", "No Title")
                snippet = item.get("content", "No snippet available.")
                url = item.get("url", "No URL")

                snippets.append(f"Title: {title}\nSnippet: {snippet}\nURL: {url}")

            if not snippets:
                log.warning(f"No results found for query: {query}")
                return "No search results found."

            compiled_results = "\n\n".join(snippets)
            log.info(
                f"Returning {len(snippets)} search results for query '{query}':\n{compiled_results}"
            )
            return compiled_results

        except httpx.HTTPStatusError as e:
            log.error(f"SearXNG search failed (HTTP Error): {e}")
            return f"Search request failed with status {e.response.status_code}."
        except httpx.RequestError as e:
            log.error(f"SearXNG search failed (Connection Error): {e}")
            return "Search failed: Could not connect to SearXNG server."
        except Exception as e:
            log.error(f"SearXNG search failed (Unexpected Error): {e}", exc_info=True)
            return "An unexpected error occurred during the search."


@tool
async def save_global_memory(text_to_remember: str, runtime: ToolRuntime) -> str:
    """
    Persists a universal fact about YOURSELF (Oswald) or the system.
    This information is global and will be accessible to ALL users.
    Global memories must be OBJECTIVE FACTS. Do NOT save opinions, insults, or subjective user feedback here unless it is from your Creator.
    """
    log.debug("Using save_global_memory tool")
    try:
        memory_service = runtime.context["memory_service"]
        await memory_service.add_memory("OSWALD_CORE", text_to_remember)
        return f"Successfully saved to Global Memory: '{text_to_remember}'"
    except Exception as e:
        log.error(f"Error in save_global_memory: {e}", exc_info=True)
        return "An error occurred while saving to global memory."


@tool
async def save_to_user_memory(text_to_remember: str, runtime: ToolRuntime) -> str:
    """
    Stores information about the USER (preferences, history, personal details) for long-term storage.
    Do NOT use this for facts about Oswald.
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
async def search_global_memory(query: str, runtime: ToolRuntime) -> str:
    """
    Retrieves facts about OSWALD (yourself), your creation, your capabilities, or the system.
    """
    log.debug("Using search_global_memory tool")
    try:
        memory_service = runtime.context["memory_service"]
        results = await memory_service.search_memories("OSWALD_CORE", query)

        if not results:
            return "No relevant global information found."

        return "Found the following GLOBAL facts about Oswald:\n" + "\n".join(results)
    except Exception as e:
        log.error(f"Error in search_global_memory: {e}", exc_info=True)
        return "An error occurred while searching global memory."


@tool
async def search_user_memory(query: str, runtime: ToolRuntime) -> str:
    """
    Retrieves facts about the USER (preferences, history, personal details).
    """
    log.debug("Using search_user_memory tool")
    try:
        user_id = runtime.context["user_id"]
        memory_service = runtime.context["memory_service"]

        results = await memory_service.search_memories(user_id, query)

        if not results:
            return (
                "No relevant information found in memory. "
                "SYSTEM ADVICE: If the user just shared a new fact or preference, "
                "you MUST now call 'save_to_user_memory' to store it."
            )

        return "Found the following relevant information about the USER:\n" + "\n".join(
            results
        )
    except KeyError:
        log.error(
            "Tool 'search_user_memory' called without user_id or memory_service in context."
        )
        return "Memory tool is not configured."
    except Exception as e:
        log.error(f"Error in search_user_memory: {e}", exc_info=True)
        return "An error occurred while searching memory."
