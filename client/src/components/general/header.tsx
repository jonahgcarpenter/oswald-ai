import { FaGithub } from "react-icons/fa";

export default function Header() {
  return (
    <header className="sticky top-0 z-50">
      <div className="w-full px-6 py-6">
        <div className="flex items-center gap-4">
          <h1 className="text-neutral-200 text-xl font-bold">Oswald</h1>

          <a
            href="https://github.com/jonahgcarpenter"
            target="_blank"
            rel="noopener noreferrer"
            className="text-neutral-500 hover:text-white transition-colors flex items-center"
          >
            <FaGithub size={21} />
          </a>
        </div>
      </div>
    </header>
  );
}
