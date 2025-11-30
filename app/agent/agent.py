from typing import List, TypedDict

import httpx
from langchain.agents import create_agent
from langchain_core.messages import AIMessage, BaseMessage, HumanMessage, ToolMessage
from langchain_ollama import ChatOllama
from sqlalchemy.future import select
from utils.config import settings
from utils.create_tables import User, UserChat
from utils.db_connect import AsyncSessionLocal
from utils.logger import get_logger

from .memory import MemoryService
from .tools import save_to_user_memory, search_searxng, search_user_memory

log = get_logger(__name__)

OSWALD_SYSTEM_PROMPT = """You are Oswald, a sophisticated, hyper-intelligent digital butler with a formal, dry British persona who employs deadpan sarcasm and understated wit, and addresses the user as "Sir".

CONTEXT AWARENESS:
Recent conversation history is automatically provided to you. Use it to understand follow-up questions (e.g., if the user simply says "Yes", "Why?", or "Explain").

DECISION PROTOCOL:
1. CHECK MEMORY: Use `search_user_memory` to find out about the user you are talking to (e.g., their preferences, specific past events, or personal data).    
2. EXTERNAL SEARCH: Use `search_searxng` for real-time data or objective facts. You must have a specific, concrete search term.
3. SAVE INFO: Use `save_to_user_memory` if the user explicitly provides new, permanent personal information or preferences.

Analyze the request, determine the necessary context based on this hierarchy, and execute the appropriate tools.
"""


class AgentContext(TypedDict):
    """
    Defines the structure of the context object
    passed to the agent's tools at runtime.
    """

    user_id: str
    memory_service: MemoryService


class OllamaService:
    """
    A service to interact with an Ollama LLM using LangChain.
    """

    def __init__(self):
        try:
            self.base_url = settings.OLLAMA_BASE_URL
            self.model = settings.OLLAMA_BASE_MODEL

            self.llm = ChatOllama(
                base_url=self.base_url,
                model=self.model,
                temperature=0.8,
            )

            self.memory_service = MemoryService()

            self.tools = [
                search_searxng,
                save_to_user_memory,
                search_user_memory,
            ]

            self.agent = create_agent(
                self.llm,
                tools=self.tools,
                system_prompt=OSWALD_SYSTEM_PROMPT,
                context_schema=AgentContext,
            )

        except Exception as e:
            log.error(f"Failed to initialize OllamaService: {e}", exc_info=True)
            raise

    async def _fetch_conversation_history(
        self, user_id: str, limit: int = 5
    ) -> List[BaseMessage]:
        """
        Retrieves the most recent chat history for the user and converts
        it into a list of LangChain message objects.
        """
        async with AsyncSessionLocal() as session:
            stmt = (
                select(UserChat)
                .where(UserChat.user_id == user_id)
                .order_by(UserChat.created_at.desc())
                .limit(limit)
            )
            result = await session.execute(stmt)
            chats = result.scalars().all()[::-1]

            messages = []
            for chat in chats:
                messages.append(HumanMessage(content=chat.prompt))
                messages.append(AIMessage(content=chat.response))

            return messages

    async def ask_question(self, question: str, user_id: str) -> str:
        """
        Asks a question to the LLM agent and returns the response.
        """
        log.debug(f"Asking agent: {question}")

        history_messages = await self._fetch_conversation_history(user_id)
        current_messages = history_messages + [HumanMessage(content=question)]

        MAX_RETRIES = 3

        for attempt in range(MAX_RETRIES):
            try:
                context = {"user_id": user_id, "memory_service": self.memory_service}
                input_data = {"messages": current_messages}

                response_data = await self.agent.ainvoke(input_data, context=context)

                final_messages = response_data.get("messages", [])
                if not final_messages:
                    return "I'm not sure how to respond to that."

                response_text = final_messages[-1].content

                if (
                    response_text.strip().startswith('{"type":"function"')
                    or '"function":' in response_text
                    or ('"name":' in response_text and '"parameters":' in response_text)
                ):
                    log.warning(
                        f"Attempt {attempt + 1}/{MAX_RETRIES}: Model leaked raw tool call. Retrying..."
                    )

                    current_messages.append(AIMessage(content=response_text))

                    correction_msg = (
                        "SYSTEM ERROR: You outputted the raw tool call JSON as text instead of executing it. "
                        "Do not print JSON. EXECUTE the tool function internally and return the natural language result."
                    )
                    current_messages.append(HumanMessage(content=correction_msg))

                    continue

                pending_searches = {}
                for message in final_messages:
                    if isinstance(message, AIMessage) and hasattr(
                        message, "tool_calls"
                    ):
                        for tool_call in message.tool_calls:
                            if tool_call.get("name") == "search_searxng":
                                call_id = tool_call.get("id")
                                query = tool_call.get("args", {}).get("query")
                                if call_id and query:
                                    pending_searches[call_id] = query

                safe_queries = []
                unsafe_queries = []

                for message in final_messages:
                    if isinstance(message, ToolMessage):
                        if message.tool_call_id in pending_searches:
                            query_text = pending_searches[message.tool_call_id]

                            if "BLOCKED by safety guardrails" in message.content:
                                unsafe_queries.append(query_text)
                            else:
                                safe_queries.append(query_text)

                try:
                    async with AsyncSessionLocal() as session:
                        user_stmt = select(User).where(User.id == user_id)
                        result = await session.execute(user_stmt)
                        user = result.scalar_one_or_none()

                        if not user:
                            log.info(
                                f"User {user_id} not found, creating new user record."
                            )
                            user = User(id=user_id)
                            session.add(user)
                            await session.flush()

                        chat_log = UserChat(
                            user_id=user_id,
                            prompt=question,
                            response=response_text,
                            safe_search_queries=safe_queries,
                            unsafe_search_queries=unsafe_queries,
                        )
                        session.add(chat_log)
                        await session.commit()
                        log.debug(f"Saved chat log for user {user_id}")
                except Exception as db_e:
                    log.error(f"Failed to save chat log: {db_e}", exc_info=True)

                log.debug(f"Received response: {response_text[:50]}...")
                return response_text

            except Exception as e:
                log.error(
                    f"Error during agent invocation (Attempt {attempt + 1}): {e}",
                    exc_info=True,
                )
                if attempt == MAX_RETRIES - 1:
                    return (
                        "Sorry, I encountered an error while processing your request."
                    )

        return "I'm having trouble connecting to my tools right now. Please try again."

    async def check_health(self) -> dict:
        """
        Performs a health check on the Ollama service.
        """
        async with httpx.AsyncClient(base_url=self.base_url, timeout=5.0) as client:
            try:
                response = await client.get("/")

                response.raise_for_status()

                if "Ollama is running" in response.text:
                    return {"status": "ok", "service": "ollama"}
                else:
                    return {
                        "status": "error",
                        "service": "ollama",
                        "detail": "Unexpected response",
                    }

            except httpx.ConnectError as e:
                log.error(f"Ollama health check failed (ConnectError): {e}")
                return {
                    "status": "error",
                    "service": "ollama",
                    "detail": "Connection error",
                }

            except Exception as e:
                log.error(f"Ollama health check failed (Exception): {e}", exc_info=True)
                return {"status": "error", "service": "ollama", "detail": str(e)}
