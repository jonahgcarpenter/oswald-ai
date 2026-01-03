from sqlalchemy.exc import OperationalError
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker, create_async_engine
from sqlalchemy.orm import DeclarativeBase
from sqlalchemy.sql import text

from .config import settings
from .logger import get_logger

log = get_logger(__name__)

_database_url = settings.DATABASE_URL
if _database_url.startswith("postgresql://"):
    _database_url = _database_url.replace("postgresql://", "postgresql+asyncpg://", 1)
elif _database_url.startswith("postgres://"):
    _database_url = _database_url.replace("postgres://", "postgresql+asyncpg://", 1)

engine = create_async_engine(
    _database_url,
    echo=False,
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
    Checks the database connection status by executing a simple ping.
    """
    try:
        await session.execute(text("SELECT 1"))

        return {
            "status": "ok",
            "database": "connected",
        }

    except OperationalError as e:
        log.error(f"Health check failed (OperationalError): {e}")
        return {"status": "error", "database": "disconnected", "error_message": str(e)}

    except Exception as e:
        log.error(f"An internal error occurred during health check: {e}", exc_info=True)
        return {
            "status": "error",
            "database": "internal_server_error",
            "error_message": str(e),
        }
