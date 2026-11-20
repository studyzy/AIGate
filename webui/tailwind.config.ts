import type { Config } from "tailwindcss";

export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        border: "hsl(214 32% 91%)",
        muted: "hsl(210 40% 96%)",
        primary: "hsl(221 83% 53%)",
      },
    },
  },
  plugins: [],
} satisfies Config;
