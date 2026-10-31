import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { AppRoot, createApp } from "./app";
import "./index.css";

const root = document.getElementById("root");
if (!root) throw new Error("#root not found");

createRoot(root).render(
  <StrictMode>
    <AppRoot app={createApp()} />
  </StrictMode>,
);
