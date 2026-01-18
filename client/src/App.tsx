import { Routes, Route } from "react-router-dom";
import Layout from "./components/general/layout";

import Home from "./pages/home";

export default function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route path="/" element={<Home />} />
      </Route>
    </Routes>
  );
}
