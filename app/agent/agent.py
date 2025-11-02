import os

import httpx
from langchain.agents import create_agent
from langchain_core.messages import HumanMessage
from langchain_ollama import ChatOllama
from utils.logger import get_logger

from .search import search_searxng
from .system_prompts import OSWALD_SYSTEM_PROMPT

log = get_logger(__name__)


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

            self.tools = [search_searxng]

            self.agent = create_agent(
                self.llm,
                tools=self.tools,
                system_prompt=OSWALD_SYSTEM_PROMPT,
            )

            log.info(f"OllamaService initialized successfully with model: {self.model}")

        except Exception as e:
            log.error(f"Failed to initialize OllamaService: {e}", exc_info=True)
            raise

    async def ask_question(self, question: str) -> str:
        """
        Asks a question to the LLM agent and returns the response.
        """
        log.debug(f"Asking agent: {question}")
        try:
            input_data = {"messages": [HumanMessage(content=question)]}

            response_data = await self.agent.ainvoke(input_data)

            final_messages = response_data.get("messages", [])
            if final_messages:
                response_text = final_messages[-1].content
            else:
                response_text = "I'm not sure how to respond to that."

            log.debug(f"Received response: {response_text[:50]}...")
            return response_text
        except Exception as e:
            log.error(f"Error during agent invocation: {e}", exc_info=True)
            return "Sorry, I encountered an error while processing your request."

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
