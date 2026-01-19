import contextlib
import json
import os
from asyncio.streams import start_unix_server

from langchain_mcp_adapters.client import MultiServerMCPClient
from langchain_mcp_adapters.tools import load_mcp_tools
from src.util.vars import settings

_EXIT_STACK = None
_GLOBAL_TOOLS = []


async def initialize_mcp_client():
    """
    Initializes the MCP Client and Docker containers.
    """
    global _EXIT_STACK, _GLOBAL_TOOLS

    if _EXIT_STACK:
        return

    config_path = "config/mcp_servers.json"
    if not os.path.exists(config_path):
        raise FileNotFoundError(f"MCP Config not found at {config_path}")

    with open(config_path, "r") as f:
        config_data = json.load(f)

    server_definitions = config_data.get("mcpServers", {})

    if "discord" in server_definitions:
        if "env" not in server_definitions["discord"]:
            server_definitions["discord"]["env"] = {}

        server_definitions["discord"]["env"]["DISCORD_TOKEN"] = settings.DISCORD_TOKEN

        current_args = server_definitions["discord"].get("args", [])
        if current_args and not os.path.isabs(current_args[0]):
            server_definitions["discord"]["args"][0] = os.path.abspath(current_args[0])

    if "memory" in server_definitions:
        if "env" not in server_definitions["memory"]:
            server_definitions["memory"]["env"] = {}

        server_definitions["memory"]["env"].update(
            {
                "DATABASE_URL": settings.DATABASE_URL,
                "OLLAMA_URL": settings.OLLAMA_URL,
                "OLLAMA_EMBEDDING_MODEL": settings.OLLAMA_EMBEDDING_MODEL,
            }
        )

        current_args = server_definitions["memory"].get("args", [])
        if current_args and not os.path.isabs(current_args[0]):
            server_definitions["memory"]["args"][0] = os.path.abspath(current_args[0])

    for name, config in server_definitions.items():
        if "command" in config and "transport" not in config:
            config["transport"] = "stdio"

    client = MultiServerMCPClient(server_definitions)
    stack = contextlib.AsyncExitStack()
    active_tools = []

    try:
        for server_name in server_definitions.keys():
            session_ctx = client.session(server_name)
            session = await stack.enter_async_context(session_ctx)
            server_tools = await load_mcp_tools(session)
            active_tools.extend(server_tools)

        _EXIT_STACK = stack
        _GLOBAL_TOOLS = active_tools

    except Exception as e:
        print(f"Failed to initialize MCP: {e}")
        await stack.aclose()
        raise e


async def get_mcp_tools():
    """
    Simple getter for the globally loaded tools.
    """
    if not _GLOBAL_TOOLS:
        print("Warning: MCP Tools were lazy-loaded. This may cause shutdown errors.")
        await initialize_mcp_client()

    return _GLOBAL_TOOLS


async def close_mcp_client():
    global _EXIT_STACK, _GLOBAL_TOOLS
    if _EXIT_STACK:
        print("Closing MCP Sessions...")
        await _EXIT_STACK.aclose()
        _EXIT_STACK = None
        _GLOBAL_TOOLS = []
