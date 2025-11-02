import os

from sqlalchemy.exc import OperationalError
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker, create_async_engine
from sqlalchemy.orm import DeclarativeBase
from sqlalchemy.sql import text

from .logger import get_logger

log = get_logger(__name__)

DB_USER = os.getenv("DB_USER")
DB_PASSWORD = os.getenv("DB_PASSWORD")
DB_HOST = os.getenv("DB_HOST")
DB_PORT = os.getenv("DB_PORT")
DB_NAME = os.getenv("DB_NAME")
DB_SCHEMA = os.getenv("DB_SCHEMA")

if not all([DB_USER, DB_PASSWORD, DB_HOST, DB_PORT, DB_NAME, DB_SCHEMA]):
    log.error("One or more database environment variables are not set.")
    exit(1)

DATABASE_URL = (
    f"postgresql+asyncpg://{DB_USER}:{DB_PASSWORD}@{DB_HOST}:{DB_PORT}/{DB_NAME}"
)

engine = create_async_engine(
    DATABASE_URL,
    echo=False,
    connect_args={"server_settings": {"search_path": DB_SCHEMA}},
)

AsyncSessionLocal = async_sessionmaker(
    bind=engine,
    class_=AsyncSession,
    expire_on_commit=False,
)


class Base(DeclarativeBase):
    pass


async def get_db_session() -> AsyncSession:
    """
    FastAPI dependency that provides a database session.
    It yields a session and handles closing it.
    """
    async with AsyncSessionLocal() as session:
        try:
            yield session
        except Exception:
            await session.rollback()
            raise
        finally:
            await session.close()


async def get_db_status(session: AsyncSession) -> dict:
    """
    Checks the database connection status
    """
    try:
        query = text(
            "SELECT 1 FROM information_schema.tables "
            "WHERE table_schema = :schema_name "
            "LIMIT 1"
        )

        result = await session.execute(query, {"schema_name": DB_SCHEMA})
        value = result.scalar()

        if value == 1:
            return {
                "status": "ok",
                "database": "connected",
            }
        else:
            return {
                "status": "error",
                "database": "connected",
                "detail": f"Schema '{DB_SCHEMA}' not found or contains no tables.",
            }

    except OperationalError as e:
        log.error(f"Health check query failed (OperationalError): {e}")
        return {"status": "error", "database": "disconnected", "error_message": str(e)}

    except Exception as e:
        log.error(f"An internal error occurred: {e}", exc_info=True)
        return {
            "status": "error",
            "database": "internal_server_error",
            "error_message": str(e),
        }
