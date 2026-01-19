import { useState, useCallback } from "react";

export type Message = {
  id: string;
  role: "user" | "assistant";
  content: string;
  logs: string[];
  isError?: boolean;
};

export function useChat() {
  const [messages, setMessages] = useState<Message[]>([]);
  const [isLoading, setIsLoading] = useState(false);

  const sendMessage = useCallback(async (prompt: string, userId: string) => {
    if (!prompt.trim()) return;

    const userMsgId = Date.now().toString();
    const aiMsgId = (Date.now() + 1).toString();

    setMessages((prev) => [
      ...prev,
      { id: userMsgId, role: "user", content: prompt, logs: [] },
      { id: aiMsgId, role: "assistant", content: "", logs: [] },
    ]);

    setIsLoading(true);

    try {
      const response = await fetch("/api/v2/chat/send", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          user_id: userId,
          prompt,
        }),
      });

      if (!response.body) throw new Error("No response body");

      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;

        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() || "";

        for (const line of lines) {
          if (!line.startsWith("data: ")) continue;
          const jsonStr = line.replace("data: ", "").trim();
          if (jsonStr === "[DONE]") break;

          try {
            const data = JSON.parse(jsonStr);

            setMessages((prev) => {
              const index = prev.findIndex((m) => m.id === aiMsgId);
              if (index === -1) return prev;

              const updatedMsg = { ...prev[index] };

              if (data.type === "token") {
                updatedMsg.content += data.content;
              } else if (data.type === "thinking" || data.type === "error") {
                updatedMsg.logs = [...updatedMsg.logs, data.content];
              }

              const newMessages = [...prev];
              newMessages[index] = updatedMsg;

              return newMessages;
            });
          } catch (e) {
            console.error("Parse Error", e);
          }
        }
      }
    } catch (err) {
      setMessages((prev) => {
        const newMessages = [...prev];
        const lastMsg = { ...newMessages[newMessages.length - 1] };
        lastMsg.logs = [...lastMsg.logs, `Error: ${(err as Error).message}`];
        lastMsg.isError = true;
        newMessages[newMessages.length - 1] = lastMsg;
        return newMessages;
      });
    } finally {
      setIsLoading(false);
    }
  }, []);

  return { messages, sendMessage, isLoading };
}
