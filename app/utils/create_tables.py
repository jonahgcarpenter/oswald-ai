import os
import uuid
from datetime import datetime

from pgvector.sqlalchemy import Vector
from sqlalchemy import Column, DateTime, ForeignKey, String
from sqlalchemy.dialects.postgresql import JSONB, UUID
from sqlalchemy.ext.asyncio import AsyncEngine
from sqlalchemy.orm import relationship
from sqlalchemy.sql import text

from .db_connect import DB_SCHEMA, Base

EMBEDDING_DIM = 768


class User(Base):
    """
    Represents a unique user that Oswald interacts with.
    """

    __tablename__ = "users"
    __table_args__ = {"schema": DB_SCHEMA}

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
    __table_args__ = {"schema": DB_SCHEMA}

    id = Column(UUID(as_uuid=True), primary_key=True, default=uuid.uuid4)
    user_id = Column(
        String, ForeignKey(f"{DB_SCHEMA}.users.id"), nullable=False, index=True
    )

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
    __table_args__ = {"schema": DB_SCHEMA}

    id = Column(UUID(as_uuid=True), primary_key=True, default=uuid.uuid4)

    user_id = Column(
        String, ForeignKey(f"{DB_SCHEMA}.users.id"), nullable=False, index=True
    )

    prompt = Column(String, nullable=False)

    response = Column(String, nullable=False)

    safe_search_queries = Column(JSONB, nullable=True, default=list)
    unsafe_search_queries = Column(JSONB, nullable=True, default=list)

    created_at = Column(DateTime, default=datetime.utcnow)

    user = relationship("User", back_populates="chats")

    def __repr__(self):
        return f"<UserChat(id='{self.id}', user_id='{self.user_id}')>"


async def create_db_and_tables(engine: AsyncEngine):
    """
    Initializes the database and creates tables.
    Also ensures the 'vector' extension is enabled.
    """
    async with engine.begin() as conn:
        await conn.execute(text(f"CREATE SCHEMA IF NOT EXISTS {DB_SCHEMA};"))

        await conn.execute(text(f"SET search_path TO {DB_SCHEMA}, public;"))

        await conn.execute(text("CREATE EXTENSION IF NOT EXISTS vector SCHEMA public;"))

        await conn.run_sync(Base.metadata.create_all)
