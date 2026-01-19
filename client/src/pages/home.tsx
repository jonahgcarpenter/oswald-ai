import { useEffect, useRef } from "react";
import Input from "../components/chat/input";
import { useChat } from "../hooks/useChat";

const CURRENT_USER_ID = "Jonah";

export default function Home() {
  const { messages, sendMessage, isLoading } = useChat();
  const messagesEndRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages]);

  const handleSendMessage = (text: string, images?: string[]) => {
    sendMessage(text, CURRENT_USER_ID, images);
  };

  return (
    <div className="flex flex-col h-full">
      <div className="flex-grow flex flex-col overflow-y-auto px-4 py-4 space-y-6 scrollbar-none">
        {messages.length === 0 && (
          <div className="flex flex-grow flex-col items-center justify-center text-neutral-500">
            <img src="/oswald-logo.png" alt="Oswald" className="w-16 h-16" />

            <p>Welcome back, {CURRENT_USER_ID}!</p>
          </div>
        )}

        {messages.map((msg) => (
          <div
            key={msg.id}
            className={`flex w-full ${
              msg.role === "user" ? "justify-end" : "justify-start"
            }`}
          >
            <div
              className={`max-w-[85%] rounded-2xl px-5 py-3 ${
                msg.role === "user"
                  ? "bg-neutral-800 text-neutral-100 rounded-br-none"
                  : "bg-transparent text-neutral-200"
              }`}
            >
              {msg.images && msg.images.length > 0 && (
                <div className="mb-3 flex flex-wrap gap-2">
                  {msg.images.map((img, idx) => (
                    <img
                      key={idx}
                      src={img}
                      alt="User uploaded content"
                      className="max-w-full h-auto rounded-lg border border-neutral-700 max-h-[300px]"
                    />
                  ))}
                </div>
              )}

              {msg.role === "assistant" && msg.logs.length > 0 && (
                <details className="scrollbar-none group mb-2 border border-neutral-800 bg-neutral-900/50 rounded-lg overflow-hidden">
                  <summary className="cursor-pointer px-3 py-2 text-xs font-mono text-neutral-500 hover:text-neutral-300 hover:bg-neutral-800/50 transition-colors flex items-center gap-2 select-none">
                    <span className="opacity-50 group-open:rotate-90 transition-transform">
                      ▶
                    </span>
                    Agent Thought Process
                  </summary>
                  <div className="px-4 py-3 bg-black/20 font-mono text-xs text-neutral-400 max-h-60 overflow-y-auto border-t border-neutral-800">
                    {msg.logs.map((log, i) => (
                      <div
                        key={i}
                        className="mb-1 border-b border-neutral-800/50 pb-1 last:border-0"
                      >
                        {log}
                      </div>
                    ))}
                  </div>
                </details>
              )}

              <div className="leading-relaxed whitespace-pre-wrap">
                {msg.content}
                {msg.role === "assistant" &&
                  isLoading &&
                  msg === messages[messages.length - 1] && (
                    <span className="inline-block w-1.5 h-4 ml-1 align-middle bg-neutral-400 animate-pulse" />
                  )}
              </div>
            </div>
          </div>
        ))}

        <div ref={messagesEndRef} />
      </div>

      <div className="w-full pt-2">
        <Input onSendMessage={handleSendMessage} isLoading={isLoading} />
      </div>
    </div>
  );
}
