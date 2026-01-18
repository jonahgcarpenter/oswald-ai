import { Outlet } from "react-router-dom";
import Header from "./header";
import Footer from "./footer";

export default function Layout() {
  return (
    <div className="min-h-screen flex flex-col bg-neutral-950">
      <Header />

      <main className="flex-grow flex flex-col container mx-auto px-4 max-w-7xl w-full">
        <Outlet />
      </main>

      <Footer />
    </div>
  );
}
