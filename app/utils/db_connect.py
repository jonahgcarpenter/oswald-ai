from sqlalchemy.exc import OperationalError
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker, create_async_engine
from sqlalchemy.orm import DeclarativeBase
from sqlalchemy.sql import text

from .config import settings
from .logger import get_logger

log = get_logger(__name__)

DB_SCHEMA = settings.DATABASE_SCHEMA

_database_url = settings.DATABASE_URL
if _database_url.startswith("postgresql://"):
    _database_url = _database_url.replace("postgresql://", "postgresql+asyncpg://", 1)
elif _database_url.startswith("postgres://"):
    _database_url = _database_url.replace("postgres://", "postgresql+asyncpg://", 1)

engine = create_async_engine(
    _database_url,
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
