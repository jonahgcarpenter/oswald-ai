import logging

from ddgs import DDGS
from mcp.server.fastmcp import FastMCP

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

mcp = FastMCP("web_search")


@mcp.tool()
def web_search(query: str, max_results: int = 5) -> str:
    """
    Performs a web search to retrieve up-to-date information, news, or technical docs.
    Use this to fill knowledge gaps or when the user asks for current events.
    """
    logger.info(f"Received search query: {query}")
    try:
        results = []
        with DDGS() as ddgs:
            gen_results = ddgs.text(query, max_results=max_results)

            for r in gen_results:
                results.append(r)

        logger.info(f"Found {len(results)} results.")

        if not results:
            return f"No results found for query: {query}"

        output = [f"--- Search Results for '{query}' ---"]
        for i, res in enumerate(results, 1):
            title = res.get("title", "No Title")
            link = res.get("href", "#")
            snippet = res.get("body", "No content.")
            output.append(f"{i}. {title}\n   Source: {link}\n   Snippet: {snippet}\n")

        return "\n".join(output)

    except Exception as e:
        logger.error(f"Search failed: {e}")
        return f"Error performing web search: {str(e)}"


if __name__ == "__main__":
    mcp.run()
