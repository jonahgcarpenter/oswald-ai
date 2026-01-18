import { useState, type KeyboardEvent } from "react";
import { FaPlus, FaArrowRight, FaMicrophone } from "react-icons/fa";

interface InputProps {
  onSendMessage?: (message: string) => void;
  isLoading?: boolean;
}

export default function Input({
  onSendMessage,
  isLoading = false,
}: InputProps) {
  const [input, setInput] = useState("");

  const handleSend = () => {
    if (!input.trim() || isLoading) return;

    if (onSendMessage) {
      onSendMessage(input);
    }

    setInput("");
  };

  const handleKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      handleSend();
    }
  };

  return (
    <div className="w-full max-w-4xl mx-auto px-4">
      <div className="relative flex items-center bg-neutral-800/50 hover:bg-neutral-800 transition-colors border border-neutral-700/50 rounded-full px-4 py-3 shadow-lg backdrop-blur-sm">
        <button
          className="cursor-pointer p-2 text-neutral-400 hover:text-neutral-200 hover:bg-neutral-700 rounded-full transition-all"
          title="Add attachment"
          onClick={() => console.log("Open attachment modal")}
        >
          <FaPlus size={16} />
        </button>

        <input
          type="text"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder="Ask Oswald anything..."
          className="flex-grow bg-transparent text-neutral-200 placeholder-neutral-500 px-4 focus:outline-none text-base"
          disabled={isLoading}
        />

        <div className="flex items-center gap-2">
          {input.trim().length === 0 ? (
            <button
              className="cursor-pointer p-2 text-neutral-400 hover:text-neutral-200 hover:bg-neutral-700 rounded-full transition-all"
              onClick={() => console.log("Start voice recording")}
            >
              <FaMicrophone size={16} />
            </button>
          ) : (
            <button
              onClick={handleSend}
              disabled={isLoading}
              className={`p-2 rounded-full transition-all ${
                isLoading
                  ? "text-neutral-600 cursor-not-allowed"
                  : "cursor-pointer text-neutral-400 hover:text-neutral-100 hover:bg-neutral-700"
              }`}
            >
              <FaArrowRight size={16} />
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
