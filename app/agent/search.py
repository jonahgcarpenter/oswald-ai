import os

import httpx
from langchain_core.tools import tool
from utils.logger import get_logger

log = get_logger(__name__)

SEARXNG_URL = os.getenv("SEARXNG_URL")

if not SEARXNG_URL:
    log.error("SEARXNG_URL environment variable not set.")


# TODO: Implement some form of gaurdrails to check search queries before execution
@tool
async def search_searxng(query: str) -> str:
    """
    Searches the web using a private SearXNG instance.
    Use this tool for recent events, facts, or information
    you do not have in your internal knowledge.
    """
    if not SEARXNG_URL:
        return "Search is not configured; SEARXNG_URL is not set."

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
