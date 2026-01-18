import asyncio
import os

import discord
from mcp.server.fastmcp import FastMCP

mcp = FastMCP("discord")

intents = discord.Intents.default()
intents.message_content = True
intents.members = True
client = discord.Client(intents=intents)


async def start_discord():
    """Helper to ensure Discord client is connected."""
    if not client.is_ready():
        token = os.getenv("DISCORD_TOKEN")
        if not token:
            raise ValueError(
                "DISCORD_TOKEN environment variable is missing! Check mcp_client.py injection."
            )

        await client.login(token)
        asyncio.create_task(client.connect())
        await client.wait_until_ready()


@mcp.tool()
async def discord_list_guilds() -> str:
    """
    List all servers (Guilds) the bot is currently connected to.
    Use this FIRST to find the 'guild_id' required for other tools.
    """
    await start_discord()

    if not client.guilds:
        return "The bot is not currently joined to any servers."

    lines = []
    for g in client.guilds:
        lines.append(f"Server: {g.name} | ID: {g.id}")

    return "\n".join(lines)


@mcp.tool()
async def discord_list_channels(guild_id: str) -> str:
    """
    List all text channels (Name & ID) in a guild.
    Use this to find the numeric ID for a channel like 'general'.
    """
    await start_discord()
    try:
        guild = client.get_guild(int(guild_id))
        if not guild:
            return f"Error: Guild {guild_id} not found. Is the bot in the server?"

        lines = []
        for c in guild.text_channels:
            lines.append(f"Name: {c.name} | ID: {c.id}")

        return "\n".join(lines) if lines else "No text channels found."
    except Exception as e:
        return f"Error listing channels: {str(e)}"


@mcp.tool()
async def discord_read_messages(channel_id: str, limit: int = 5) -> str:
    """Read the last N messages from a channel."""
    await start_discord()
    try:
        channel = client.get_channel(int(channel_id))
        if not channel:
            return f"Error: Channel {channel_id} not found."

        msgs = []
        async for m in channel.history(limit=limit):
            msgs.append(f"{m.author.name}: {m.content}")

        return "\n".join(msgs[::-1]) if msgs else "No messages found."
    except Exception as e:
        return f"Error reading messages: {str(e)}"


@mcp.tool()
async def discord_get_server_info(guild_id: str) -> str:
    """Get basic server info."""
    await start_discord()
    try:
        guild = client.get_guild(int(guild_id))
        if not guild:
            return "Guild not found."
        return f"Server: {guild.name} | Members: {guild.member_count} | ID: {guild.id}"
    except Exception as e:
        return f"Error getting info: {str(e)}"


@mcp.tool()
async def discord_lookup_user(user_id: str) -> str:
    """
    Lookup a user by their ID to get their real username and display name.
    Accepts raw IDs (e.g. "255088415479955457") or mentions (e.g. "<@255088415479955457>").
    """
    await start_discord()

    clean_id = user_id.replace("<@", "").replace("!", "").replace(">", "").strip()

    if not clean_id.isdigit():
        return f"Error: '{user_id}' is not a valid User ID format."

    try:
        user = await client.fetch_user(int(clean_id))

        return (
            f"--- User Lookup ---\n"
            f"ID: {user.id}\n"
            f"Username: {user.name}\n"
            f"Global Display Name: {user.global_name or 'None'}\n"
            f"Is Bot: {user.bot}"
        )
    except discord.NotFound:
        return f"Error: User with ID {clean_id} not found."
    except Exception as e:
        return f"Error looking up user: {str(e)}"


@mcp.tool()
async def discord_send_message(channel_id: str, content: str) -> str:
    """
    Send a message to a specific channel.
    Requires a valid channel_id (use discord_list_channels first).
    """
    await start_discord()
    try:
        channel = client.get_channel(int(channel_id))
        if not channel:
            return f"Error: Channel {channel_id} not found or bot lacks access."

        sent_msg = await channel.send(content)
        return f"Message sent successfully! (ID: {sent_msg.id})"
    except Exception as e:
        return f"Error sending message: {str(e)}"


@mcp.tool()
async def discord_list_users(guild_id: str, query: str = None) -> str:
    """
    List or search for users in a guild by name/display name.
    Use this to find a User ID when you only know the name (e.g. 'Jonah').
    """
    await start_discord()
    try:
        guild = client.get_guild(int(guild_id))
        if not guild:
            return f"Error: Guild {guild_id} not found."

        matches = []
        search_term = query.lower() if query else ""

        for m in guild.members:
            if (
                (not search_term)
                or (search_term in m.name.lower())
                or (m.global_name and search_term in m.global_name.lower())
                or (m.display_name and search_term in m.display_name.lower())
            ):

                matches.append(
                    f"Name: {m.name} | Display: {m.display_name} | ID: {m.id} | Bot: {m.bot}"
                )

        if not matches:
            return f"No users found matching '{query}'."

        return "\n".join(matches[:20]) + (
            f"\n...and {len(matches)-20} more." if len(matches) > 20 else ""
        )

    except Exception as e:
        return f"Error listing users: {str(e)}"


if __name__ == "__main__":
    mcp.run()
