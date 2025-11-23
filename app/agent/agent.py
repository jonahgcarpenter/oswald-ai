import os
from typing import TypedDict

import httpx
from langchain.agents import create_agent
from langchain_core.messages import AIMessage, HumanMessage
from langchain_ollama import ChatOllama
from utils.logger import get_logger

from .memory import MemoryService, save_to_user_memory, search_user_memory
from .search import search_searxng
from .system_prompts import OSWALD_SYSTEM_PROMPT

log = get_logger(__name__)


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
        log.info("Initializing OllamaService...")
        try:
            self.base_url = os.getenv("OLLAMA_BASE_URL")
            self.model = os.getenv("OLLAMA_BASE_MODEL")

            if not self.base_url or not self.model:
                log.error("OLLAMA_BASE_URL or OLLAMA_BASE_MODEL env variables not set.")
                raise ValueError("Ollama URL or Model not set")

            self.llm = ChatOllama(base_url=self.base_url, model=self.model)

            self.memory_service = MemoryService()

            self.tools = [search_searxng, save_to_user_memory, search_user_memory]

            self.agent = create_agent(
                self.llm,
                tools=self.tools,
                system_prompt=OSWALD_SYSTEM_PROMPT,
                context_schema=AgentContext,
            )

            log.info(f"OllamaService initialized successfully with model: {self.model}")

        except Exception as e:
            log.error(f"Failed to initialize OllamaService: {e}", exc_info=True)
            raise

    async def ask_question(self, question: str, user_id: str) -> str:
        """
        Asks a question to the LLM agent and returns the response.
        """
        log.debug(f"Asking agent (User: {user_id}): {question}")

        current_messages = [HumanMessage(content=question)]
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
