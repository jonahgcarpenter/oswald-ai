import os

import httpx
from langchain_core.output_parsers import StrOutputParser
from langchain_core.prompts import ChatPromptTemplate
from langchain_ollama import OllamaLLM
from utils.logger import get_logger

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

            self.llm = OllamaLLM(base_url=self.base_url, model=self.model)

            self.prompt = ChatPromptTemplate.from_messages(
                [
                    (
                        "system",
                        OSWALD_SYSTEM_PROMPT,
                    ),
                    ("human", "{question}"),
                ]
            )

            self.parser = StrOutputParser()

            self.chain = self.prompt | self.llm | self.parser

            log.info(
                f"OllamaService initialized successfully with base model: {self.model}"
            )

        except Exception as e:
            log.error(f"Failed to initialize OllamaService: {e}", exc_info=True)
            raise

    async def ask_question(self, question: str) -> str:
        """
        Asks a question to the LLM and returns the response asynchronously.
        """
        log.debug(f"Asking question: {question}")
        try:
            response = await self.chain.ainvoke({"question": question})
            log.debug(f"Received response: {response[:50]}...")
            return response
        except Exception as e:
            log.error(f"Error during chain invocation: {e}", exc_info=True)
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
