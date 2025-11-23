import contextvars
import logging
import os

from dotenv import load_dotenv

load_dotenv()

LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()

user_context = contextvars.ContextVar("user_id", default=None)


class UserIDFilter(logging.Filter):
    """
    Injects the user_id from contextvars into the log record.
    """

    def filter(self, record):
        user_id = user_context.get()
        if user_id:
            record.user_info = f" [USER: {user_id}] "
        else:
            record.user_info = " "
        return True


LOGGING_CONFIG = {
    "version": 1,
    "disable_existing_loggers": False,
    "filters": {
        "user_id_filter": {
            "()": UserIDFilter,
        }
    },
    "formatters": {
        "color": {
            "()": "colorlog.ColoredFormatter",
            "format": "%(log_color)s%(asctime)s - %(levelname)-8s - [%(name)s]%(user_info)s- %(message)s",
            "datefmt": "%Y-%m-%d %H:%M:%S",
            "log_colors": {
                "DEBUG": "cyan",
                "INFO": "green",
                "WARNING": "yellow",
                "ERROR": "red",
                "CRITICAL": "bold_red",
            },
        },
        "simple": {
            "format": "%(asctime)s - %(levelname)-8s - [%(name)s]%(user_info)s- %(message)s",
            "datefmt": "%Y-%m-%d %H:%M:%S",
        },
    },
    "handlers": {
        "console": {
            "level": LOG_LEVEL,
            "class": "logging.StreamHandler",
            "formatter": "color",
            "stream": "ext://sys.stdout",
            "filters": ["user_id_filter"],
        },
    },
    "loggers": {
        "": {
            "level": LOG_LEVEL,
            "handlers": ["console"],
            "propagate": False,
        },
        "uvicorn": {
            "level": "WARNING",
            "handlers": ["console"],
            "propagate": False,
        },
        "uvicorn.error": {
            "level": "WARNING",
            "handlers": ["console"],
            "propagate": False,
        },
        "uvicorn.access": {
            "level": "WARNING",
            "handlers": ["console"],
            "propagate": False,
        },
        "agent": {
            "level": LOG_LEVEL,
            "handlers": ["console"],
            "propagate": False,
        },
        "sqlalchemy.engine": {
            "level": "WARNING",
            "handlers": ["console"],
            "propagate": False,
        },
        "httpcore": {
            "level": "WARNING",
            "handlers": ["console"],
            "propagate": False,
        },
        "httpx": {
            "level": "WARNING",
            "handlers": ["console"],
            "propagate": False,
        },
    },
}


def get_logger(name: str) -> logging.Logger:
    return logging.getLogger(name)
