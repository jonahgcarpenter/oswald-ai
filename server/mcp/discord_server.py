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
    Lists all connected servers with their IDs. Run this first to find a 'guild_id'.
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
    Lists text channels (Name & ID) in a specific guild. Use this to find a numeric 'channel_id'.
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
    """
    Retrieves the last N text messages from a specific channel ID.
    """
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
    """
    Returns metadata (member count, ID, name) for a specific Guild ID.
    """
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
    Resolves a User ID or mention string (e.g. <@123>) to a real username and details.
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
    Sends a text message to a specific Channel ID.
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
    Search guild members by name or display name to find their User ID.
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
