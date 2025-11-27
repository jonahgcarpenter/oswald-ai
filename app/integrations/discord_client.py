import asyncio
import logging
import os

import aiohttp
import discord
from discord.ext import commands
from utils.config import settings
from utils.logger import get_logger

TOKEN = settings.DISCORD_TOKEN
API_BASE_URL = "http://localhost:8000"
API_WRAPPER_URL = f"{API_BASE_URL}/agent/generate"
API_HEALTH_URL = f"{API_BASE_URL}/health"

log = get_logger(__name__)


logging.getLogger("discord").setLevel(logging.WARNING)
logging.getLogger("discord.client").setLevel(logging.WARNING)
logging.getLogger("discord.gateway").setLevel(logging.WARNING)


intents = discord.Intents.default()
intents.message_content = True
bot = commands.Bot(command_prefix="!", intents=intents)


@bot.event
async def on_ready():
    """Fires when connected to Discord, then checks for backend readiness."""
    logging.info(f"Connected to Discord as {bot.user}. Waiting for backend API...")

    max_retries = 12
    async with aiohttp.ClientSession() as session:
        for attempt in range(max_retries):
            try:
                async with session.get(API_HEALTH_URL, timeout=3) as response:
                    if response.status == 200:
                        data = await response.json()
                        if data.get("status") == "ok":
                            logging.info("Backend API is online and healthy")
                            logging.info(f"Bot is ready! Logged in as {bot.user}")
                            return
            except (aiohttp.ClientConnectorError, asyncio.TimeoutError):
                pass
            except aiohttp.ClientError as e:
                logging.warning(f"API health check failed: {e}")

            await asyncio.sleep(5)

    logging.critical(
        "FATAL: Backend API did not become healthy. Bot may not function correctly."
    )


@bot.event
async def on_message(message: discord.Message):
    """Fires on every message sent in a channel the bot can see."""
    if message.author == bot.user:
        return

    if (
        message.mention_everyone
        or "@everyone" in message.content
        or "@here" in message.content
    ):
        return

    if bot.user in message.mentions:
        mention_standard = f"<@{bot.user.id}>"
        mention_nickname = f"<@!{bot.user.id}>"
        prompt = (
            message.content.replace(mention_standard, "")
            .replace(mention_nickname, "")
            .strip()
        )

        if not prompt:
            await message.reply("What the fuck do you want idiot?")
            return

        if message.mentions:
            for member in message.mentions:
                if member.id != bot.user.id:
                    prompt = prompt.replace(member.mention, member.display_name)

        async with message.channel.typing():
            try:
                payload = {"question": prompt, "user_id": str(message.author.name)}

                async with aiohttp.ClientSession() as session:
                    async with session.post(
                        API_WRAPPER_URL, json=payload, timeout=70
                    ) as response:
                        response.raise_for_status()
                        api_data = await response.json()
                        model_response = api_data.get(
                            "answer", "Sorry, I received an empty response."
                        )

                if len(model_response) > 2000:
                    logging.warning("Response > 2000 chars, splitting.")
                    for i in range(0, len(model_response), 1990):
                        chunk = model_response[i : i + 1990]
                        if i == 0:
                            await message.reply(chunk)
                        else:
                            await message.channel.send(chunk)
                else:
                    await message.reply(model_response)

            except aiohttp.ClientResponseError as http_err:
                error_detail = "An unknown error occurred."
                try:
                    error_json = await http_err.json()
                    error_detail = error_json.get("detail", error_detail)
                except Exception:
                    pass
                await message.reply(f"An error occurred with the API: {error_detail}")
                logging.error(f"HTTPError: {error_detail} (Status: {http_err.status})")
            except asyncio.TimeoutError:
                await message.reply(
                    "My brain took too long to respond (timeout). Please try again."
                )
                logging.error("API Connection Error: Timeout")
            except aiohttp.ClientConnectorError as e:
                await message.reply(
                    "I couldn't connect to my brain (the API wrapper). Please check if it's running."
                )
                logging.error(f"API Connection Error: {e}")
            except Exception as e:
                await message.reply(
                    "An unexpected error occurred. Please check the logs."
                )
                logging.error(f"Unexpected error in on_message: {e}", exc_info=True)


bot.run(TOKEN)
