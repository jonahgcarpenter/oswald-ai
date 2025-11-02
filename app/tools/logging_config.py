import logging
import os

from dotenv import load_dotenv
from rich.logging import RichHandler


def setup_logging():
    """
    Sets up a configurable logger using Rich for beautiful, colored output.
    """
    load_dotenv()
    log_level_str = os.getenv("LOG_LEVEL", "INFO").upper()
    log_level = getattr(logging, log_level_str, logging.INFO)

    handler = RichHandler(
        rich_tracebacks=True,
        markup=True,
        show_path=False,
    )

    root_logger = logging.getLogger()
    root_logger.setLevel(log_level)
    if root_logger.hasHandlers():
        root_logger.handlers.clear()

    root_logger.addHandler(handler)

    logging.getLogger("discord").setLevel(logging.WARNING)
    logging.getLogger("discord.client").setLevel(logging.WARNING)
    logging.getLogger("discord.gateway").setLevel(logging.WARNING)
    logging.getLogger("sentence_transformers").setLevel(logging.WARNING)
    logging.getLogger("transformers.modeling_utils").setLevel(logging.ERROR)

    logger = logging.getLogger(__name__)
    logger.info(f"Logging initialized with level: {log_level_str}")
