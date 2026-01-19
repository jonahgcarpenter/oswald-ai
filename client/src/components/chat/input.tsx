import { useState, useRef, type KeyboardEvent, type ChangeEvent } from "react";
import { FaPlus, FaArrowRight, FaTimes } from "react-icons/fa";

interface InputProps {
  onSendMessage?: (message: string, images?: string[]) => void;
  isLoading?: boolean;
}

export default function Input({
  onSendMessage,
  isLoading = false,
}: InputProps) {
  const [input, setInput] = useState("");
  const [selectedImage, setSelectedImage] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const handleFileChange = (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (file) {
      const reader = new FileReader();
      reader.onloadend = () => {
        const base64String = reader.result as string;
        setSelectedImage(base64String);
      };
      reader.readAsDataURL(file);
    }
  };

  const handleSend = () => {
    if ((!input.trim() && !selectedImage) || isLoading) return;

    if (onSendMessage) {
      const images = selectedImage ? [selectedImage] : [];
      onSendMessage(input, images);
    }

    setInput("");
    setSelectedImage(null);
    if (fileInputRef.current) fileInputRef.current.value = "";
  };

  const handleKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      handleSend();
    }
  };

  return (
    <div className="w-full max-w-4xl mx-auto px-4">
      {selectedImage && (
        <div className="mb-2 relative w-fit">
          <img
            src={selectedImage}
            alt="Preview"
            className="h-20 rounded-lg border border-neutral-700"
          />
          <button
            onClick={() => setSelectedImage(null)}
            className="absolute -top-2 -right-2 bg-red-500 rounded-full p-1 text-white hover:bg-red-600"
          >
            <FaTimes size={10} />
          </button>
        </div>
      )}

      <div className="relative flex items-center bg-neutral-800/50 hover:bg-neutral-800 transition-colors border border-neutral-700/50 rounded-full px-4 py-3 shadow-lg backdrop-blur-sm">
        <input
          type="file"
          ref={fileInputRef}
          onChange={handleFileChange}
          accept="image/*"
          className="hidden"
        />

        <button
          className="cursor-pointer p-2 text-neutral-400 hover:text-neutral-200 hover:bg-neutral-700 rounded-full transition-all"
          title="Add attachment"
          onClick={() => fileInputRef.current?.click()}
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
        </div>
      </div>
    </div>
  );
}
