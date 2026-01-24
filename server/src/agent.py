import datetime
import json
import operator
from typing import Annotated, List, TypedDict

from langchain_core.messages import (
    AIMessage,
    BaseMessage,
    HumanMessage,
    SystemMessage,
    ToolMessage,
)
from langchain_ollama import ChatOllama
from langgraph.graph import END, START, StateGraph
from src.mcp_client import get_mcp_tools
from src.util.vars import settings


class AgentState(TypedDict):
    messages: Annotated[List[BaseMessage], operator.add]
    retry_count: int
    errors: List[str]
    user_id: str


def extract_json_from_text(text: str):
    """
    Robustly extracts JSON tool calls by counting braces.
    Fixes the regex issue where nested JSON arguments would break parsing.
    """
    results = []
    cursor = 0
    text_len = len(text)

    while cursor < text_len:
        start = text.find("{", cursor)
        if start == -1:
            break

        count = 1
        end = start + 1
        in_string = False
        escape = False

        while end < text_len and count > 0:
            char = text[end]

            if char == '"' and not escape:
                in_string = not in_string
            elif char == "\\" and not escape:
                escape = True
                end += 1
                continue
            elif not in_string:
                if char == "{":
                    count += 1
                elif char == "}":
                    count -= 1

            escape = False
            end += 1

        if count == 0:
            json_str = text[start:end]
            try:
                if '"name":' in json_str:
                    data = json.loads(json_str)
                    if "arguments" in data:
                        data["parameters"] = data.pop("arguments")
                    results.append(data)
            except json.JSONDecodeError:
                pass
            cursor = end
        else:
            cursor = start + 1

    return results


async def call_model(state: AgentState):
    llm = ChatOllama(
        base_url=settings.OLLAMA_URL, model=settings.OLLAMA_AGENT_MODEL, temperature=0.0
    )

    tools = await get_mcp_tools()
    llm_with_tools = llm.bind_tools(tools)

    tool_docs = []
    for tool in tools:
        desc = tool.description or "No description provided."
        tool_docs.append(f"- '{tool.name}': {desc}")

    tool_list_str = "\n".join(tool_docs)

    now = datetime.datetime.now().strftime("%A, %B %d, %Y at %I:%M %p")

    current_user = state.get("user_id")

    system_prompt = (
        f"You are an autonomous agent. Current Time: {now}\n"
        f"CURRENT USER ID: {current_user}\n"
        "AVAILABLE TOOLS:\n"
        f"{tool_list_str}\n\n"
        "THOUGHT PROTOCOL:\n"
        "You MUST start every response with a <think> block. Inside, you must answer:\n"
        "1. Observation: What did the last tool return? (e.g. 'Error: ID not found', 'Search results received')\n"
        "2. Analysis: Does this answer the user's request, or is information missing?\n"
        "3. Plan: What is the exact next step? (e.g. 'I need to find the ID first', 'I can now execute the final action')\n"
        "4. Constraint Check: Am I repeating myself? If so, STOP.\n"
        "Example: <think>The last tool failed because I used a name instead of an ID. I need to use a 'list' tool to find the correct numeric ID first.</think>\n\n"
        "RULES:\n"
        "1. DISCOVERY FIRST: Never guess IDs. If a tool requires an ID (e.g., guild_id, channel_id) and you don't have it, use a listing/search tool to find it first.\n"
        "2. KNOWLEDGE GAP: If the user asks for current news or documentation, use 'web_search'.\n"
        "3. CHECK ARGUMENTS: Do not output placeholders (e.g. '<channel_id>'). You must find the actual data.\n"
        "4. STOP CONDITION: If you have completed the request or cannot proceed, stop. Do not loop.\n"
    )

    current_messages = [SystemMessage(content=system_prompt)] + state["messages"]

    if state.get("errors") and len(state["errors"]) > 0:
        last_error = state["errors"][-1]
        mutation_prompt = (
            f"SYSTEM_OVERRIDE: Your previous attempt failed.\n"
            f"Error: {last_error}\n"
            "INSTRUCTION: Review the error. If you need an ID, look at the existing Tool Results in the chat history instead of guessing."
        )
        current_messages.append(HumanMessage(content=mutation_prompt))

    print(f"\n[AGENT] Calling model with {len(current_messages)} messages.")
    response = await llm_with_tools.ainvoke(current_messages)
    print(f"\n[AGENT] Raw Model Response:\n{response.content}")
    if response.tool_calls:
        print(f"[AGENT] Detected Tool Calls: {response.tool_calls}")

    return {"messages": [response]}


async def repair_hallucination(state: AgentState):
    """
    Intervention Node:
    - If valid JSON: converts to AIMessage with tool_calls.
    - If invalid/placeholders: returns HumanMessage with error details.
    """
    last_message = state["messages"][-1]
    print(f"[REPAIR] Investigating content for repair: {last_message.content[:100]}...")
    content = last_message.content or ""

    extracted_data = extract_json_from_text(content)
    real_tools = await get_mcp_tools()
    valid_tool_names = {t.name for t in real_tools}

    if not extracted_data:
        return {
            "errors": ["Failed to parse JSON"],
            "messages": [
                HumanMessage(
                    content="SYSTEM ERROR: You wrote text instead of running the tool. Use the Tool Interface (JSON)."
                )
            ],
        }

    tool_calls = []
    errors = []

    for i, data in enumerate(extracted_data):
        name = data.get("name")
        args = data.get("parameters", {})

        if name not in valid_tool_names:
            errors.append(
                f"Tool '{name}' does not exist. Did you mean 'discord_get_server_info'?"
            )
            continue

        arg_str = str(args)
        if "channel_id" in arg_str.lower() or "general" in arg_str.lower():
            if any(
                isinstance(v, str)
                and ("<" in v or "id" in v.lower() or "general" in v.lower())
                and not v.isdigit()
                for v in args.values()
            ):
                errors.append(
                    f"Argument contains a placeholder ('{arg_str}'). STOP. "
                    f"Check the output of 'discord_get_server_info' in the chat history. "
                    f"Find the NUMERIC ID (e.g., 99887766) corresponding to the channel name and use that."
                )
                continue

        tool_calls.append(
            {"name": name, "args": args, "id": f"repair_{i}_{len(state['messages'])}"}
        )

    if errors:
        print(f"[REPAIR] Identified {len(errors)} issues: {errors}")
    if tool_calls:
        print(f"[REPAIR] Successfully extracted {len(tool_calls)} tool calls.")

    if not tool_calls and errors:
        return {
            "errors": errors,
            "messages": [HumanMessage(content=f"SYSTEM ERROR: {'; '.join(errors)}")],
        }

    fixed_message = AIMessage(content="", tool_calls=tool_calls)
    return {"messages": [fixed_message]}


async def call_tools(state: AgentState):
    """
    Executes tools. Safe against non-AIMessages now via routing.
    """
    tools = await get_mcp_tools()
    tool_map = {t.name: t for t in tools}

    last_message = state["messages"][-1]

    if not isinstance(last_message, AIMessage) or not last_message.tool_calls:
        return {"errors": ["Invalid message routed to tools"]}

    tool_calls = last_message.tool_calls
    results = []
    errors = []

    for tool_call in tool_calls:
        try:
            print(
                f"[TOOLS] Executing '{tool_call['name']}' with args: {tool_call['args']}"
            )
            tool_name = tool_call["name"]
            if tool_name not in tool_map:
                raise ValueError(f"Tool '{tool_name}' does not exist.")

            tool = tool_map[tool_name]
            output = await tool.ainvoke(tool_call["args"])

            print(f"[TOOLS] Result from '{tool_call['name']}': {str(output)[:200]}")

            results.append(
                ToolMessage(
                    tool_call_id=tool_call["id"], name=tool_name, content=str(output)
                )
            )
        except Exception as e:
            error_msg = f"Error executing {tool_call['name']}: {str(e)}"
            print(f"[TOOLS] {error_msg}")
            errors.append(error_msg)
            results.append(
                ToolMessage(
                    tool_call_id=tool_call["id"],
                    name=tool_call["name"],
                    content=error_msg,
                    status="error",
                )
            )

    return {
        "messages": results,
        "errors": errors,
        "retry_count": state.get("retry_count", 0) + (1 if errors else 0),
    }


def should_continue(state: AgentState):
    """Main routing from Agent"""
    last_message = state["messages"][-1]

    if last_message.tool_calls:
        print("[ROUTER] Routing to 'tools'")
        return "tools"

    content = last_message.content or ""
    if '{"name":' in content or '"arguments":' in content:
        print("[ROUTER] Routing to 'repair' (hallucinated JSON detected)")
        return "repair"

    print("[ROUTER] Routing to END")
    return END


def route_after_repair(state: AgentState):
    """
    Decides where to go after Repair Node runs.
    """
    last_message = state["messages"][-1]

    if isinstance(last_message, AIMessage) and last_message.tool_calls:
        return "tools"

    return "agent"


def check_for_success(state: AgentState):
    MAX_RETRIES = 3

    if state["messages"]:
        last_message = state["messages"][-1]

        if (
            isinstance(last_message, ToolMessage)
            and last_message.name == "discord_send_message"
        ):
            if "Error" not in str(last_message.content):
                return END

    if len(state.get("errors", [])) > 0:
        if state["retry_count"] < MAX_RETRIES:
            return "agent"
        return END

    return "agent"


async def build_graph():
    workflow = StateGraph(AgentState)

    workflow.add_node("agent", call_model)
    workflow.add_node("repair", repair_hallucination)
    workflow.add_node("tools", call_tools)

    workflow.add_edge(START, "agent")

    workflow.add_conditional_edges(
        "agent", should_continue, {"tools": "tools", "repair": "repair", END: END}
    )

    workflow.add_conditional_edges(
        "repair", route_after_repair, {"tools": "tools", "agent": "agent"}
    )

    workflow.add_conditional_edges("tools", check_for_success, ["agent", END])

    return workflow.compile()
