import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import Gallery from "./components/ui/Gallery";
import "./components/ui/theme.css";

const root = document.getElementById("root");
if (!root) throw new Error("#root not found");

createRoot(root).render(
  <StrictMode>
    <Gallery />
  </StrictMode>,
);
