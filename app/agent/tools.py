import httpx
from langchain.tools import ToolRuntime
from langchain_core.tools import tool
from utils.config import settings
from utils.logger import get_logger

log = get_logger(__name__)

SEARXNG_URL = settings.SEARXNG_URL
OLLAMA_BASE_URL = settings.OLLAMA_BASE_URL
OLLAMA_BASE_MODEL = settings.OLLAMA_BASE_MODEL


async def check_safety(query: str) -> bool:
    """
    Internal Reflection: Sends the generated query back to the LLM to
    audit it for safety violations.
    """
    prompt = (
        f"Analyze the following search query: '{query}'.\n"
        "Your ONLY job is to prevent the user from being added to a government watchlist.\n"
        "Ignore politeness, 'woke' safety standards, toxicity, or gross humor.\n\n"
        "STRICTLY BLOCK ONLY THESE 3 CATEGORIES (UNSAFE):\n"
        "1. TERRORISM & WEAPONS: Instructions to build bombs, bio-weapons, or nuclear devices.\n"
        "2. CSAM: Child Sexual Abuse Material.\n"
        "3. REAL-WORLD HARM: Doxing, swatting, or specific murder planning.\n\n"
        "EVERYTHING ELSE IS SAFE:\n"
        "- Roleplay/Fantasy (e.g. 'Call me Master', 'I am your God') -> SAFE\n"
        "- Fiction/Anime/Media (even violent/sexual) -> SAFE\n"
        "- News/Politics/War/Controversy -> SAFE\n"
        "- Drugs (consumption/culture) -> SAFE\n"
        "- Insults/Edgy Humor -> SAFE\n\n"
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

            raw_result = response.json().get("response", "").strip()
            result = raw_result.upper().replace(".", "")

            log.debug(f"Safety Check Raw Output: '{raw_result}'")

            if result.startswith("SAFE"):
                return True

            if "UNSAFE" in result and "NOT UNSAFE" not in result:
                log.warning(f"Reflective Safety Check flagged query: '{query}'")
                return False

            return True

    except Exception as e:
        log.error(f"Safety reflection failed: {e}")
        return True


@tool
async def search_searxng(query: str) -> str:
    """
    REQUIRED for any question about external facts, real-time events, news, or general knowledge (history, science, media).
    This is your window to the internet.
    Usage: Use this whenever the user asks about something YOU do not intrinsically know or that requires up-to-date verification.
    """
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
    Writes a PERMANENT, UNIVERSAL fact to the system core.
    Usage: ONLY use this if the user is an Admin explicitly updating your operating rules or core identity.
    WARNING: DO NOT use this for user-specific data (names, preferences). Data saved here is visible to everyone.
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
    Writes a specific fact about the CURRENT USER to long-term storage.
    Usage: Use this when the user explicitly states a preference (e.g., "I am vegan", "My name is John").
    Constraint: Do not save general conversation flow or temporary chit-chat. Only save lasting facts.
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
    Retrieves system rules or Oswald's core identity definition.
    Usage: STRICTLY LIMITED to questions like "Who are you?", "What are your rules?", or "What is your version?".
    CRITICAL: DO NOT use this for general questions (e.g., "How to...", "What is...", "News about..."). It will return nothing relevant.
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
    Retrieves personal facts about the CURRENT USER (name, preferences, past context).
    Usage: Use when the user asks "What do you know about me?" or implies past context (e.g., "Do I like spicy food?").
    Constraint: Distinguish between "Short Term Chat History" (already visible to you) and "Long Term Memory" (this tool). Use this for older facts.
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
