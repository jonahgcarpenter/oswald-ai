import uuid
from datetime import datetime

from pgvector.sqlalchemy import Vector
from sqlalchemy import Column, DateTime, ForeignKey, String
from sqlalchemy.dialects.postgresql import JSONB, UUID
from sqlalchemy.exc import SQLAlchemyError
from sqlalchemy.ext.asyncio import AsyncEngine
from sqlalchemy.orm import relationship
from sqlalchemy.sql import text

from .db_connect import Base
from .logger import get_logger

log = get_logger(__name__)

EMBEDDING_DIM = 768


class User(Base):
    """
    Represents a unique user that Oswald interacts with.
    """

    __tablename__ = "users"

    id = Column(String, primary_key=True)
    created_at = Column(DateTime, default=datetime.utcnow)

    memories = relationship(
        "UserMemory",
        back_populates="user",
        cascade="all, delete-orphan",
    )

    chats = relationship(
        "UserChat",
        back_populates="user",
        cascade="all, delete-orphan",
    )

    def __repr__(self):
        return f"<User(id='{self.id}')>"


class UserMemory(Base):
    """
    Stores a single "memory" (a piece of text and its vector)
    for a specific user.
    """

    __tablename__ = "user_memories"

    id = Column(UUID(as_uuid=True), primary_key=True, default=uuid.uuid4)

    user_id = Column(String, ForeignKey("users.id"), nullable=False, index=True)

    content = Column(String, nullable=False)
    embedding = Column(Vector(EMBEDDING_DIM))

    created_at = Column(DateTime, default=datetime.utcnow, index=True)
    last_accessed_at = Column(DateTime, default=datetime.utcnow)

    user = relationship("User", back_populates="memories")

    def __repr__(self):
        return (
            f"<UserMemory(user_id='{self.user_id}', content='{self.content[:30]}...')>"
        )


class UserChat(Base):
    """
    Logs individual chat interactions including prompts, responses,
    and any search queries executed during the turn.
    """

    __tablename__ = "user_chats"

    id = Column(UUID(as_uuid=True), primary_key=True, default=uuid.uuid4)

    user_id = Column(String, ForeignKey("users.id"), nullable=False, index=True)

    prompt = Column(String, nullable=False)

    response = Column(String, nullable=False)

    search_queries = Column(JSONB, nullable=True, default=list)

    created_at = Column(DateTime, default=datetime.utcnow)

    user = relationship("User", back_populates="chats")

    def __repr__(self):
        return f"<UserChat(id='{self.id}', user_id='{self.user_id}')>"


async def create_db_and_tables(engine: AsyncEngine):
    """
    Initializes the database and creates tables.
    Ensures the 'vector' extension is enabled and logs any errors.
    """
    try:
        async with engine.begin() as conn:
            log.info("Checking for 'vector' extension...")
            await conn.execute(text("CREATE EXTENSION IF NOT EXISTS vector;"))

            log.info("Creating database tables...")
            await conn.run_sync(Base.metadata.create_all)

        log.info("Database initialization completed successfully.")

    except SQLAlchemyError as e:
        log.critical(f"Database initialization failed (SQLAlchemyError): {e}")
        raise
    except Exception as e:
        log.critical(
            f"Unexpected error during database initialization: {e}", exc_info=True
        )
        raise
