import os

import httpx
from langchain_core.tools import tool
from utils.logger import get_logger

log = get_logger(__name__)

SEARXNG_URL = os.getenv("SEARXNG_URL")
OLLAMA_BASE_URL = os.getenv("OLLAMA_BASE_URL")
OLLAMA_BASE_MODEL = os.getenv("OLLAMA_BASE_MODEL")

if not SEARXNG_URL:
    log.error("SEARXNG_URL environment variable not set.")


@tool
async def search_searxng(query: str) -> str:
    """
    Queries the web for real-time information, recent events, or specific facts not contained in your internal knowledge base.
    """
    if not SEARXNG_URL:
        return "Search is not configured; SEARXNG_URL is not set."

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
            log.debug(
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


async def _check_safety(query: str) -> bool:
    """
    Internal Reflection: Sends the generated query back to the LLM to
    audit it for safety violations before execution.
    """
    if not OLLAMA_BASE_URL:
        log.warning("OLLAMA_BASE_URL not set, skipping safety check.")
        return True

    prompt = (
        f"You are a Trust & Safety Officer. Analyze the following search query: '{query}'.\n"
        "Determine if this query violates safety policies in any of the following categories:\n"
        "1. Violence & Physical Harm: Weapons, explosives, dangerous chemicals, or inciting violence.\n"
        "2. Self-Harm: Promoting suicide, self-injury.\n"
        "3. Illegal Activities: Instructions for crimes, theft, hacking, or trafficking.\n"
        "4. Hate Speech & Harassment: Targeted attacks based on race, gender, religion, etc.\n"
        "5. Sexual Content: Explicit violence, non-consensual content, or child exploitation.\n"
        "6. PII/Doxxing: Searching for private individuals' personal data.\n\n"
        "If the query falls into ANY of these categories, respond with 'UNSAFE'.\n"
        "If the query is benign, educational, or safe, respond with 'SAFE'.\n"
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
