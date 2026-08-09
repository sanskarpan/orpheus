import type { Config } from "tailwindcss";

/**
 * Studio Console theme. Near-black ground, brass/amber accent, waveform motifs.
 * Colors are also mirrored as CSS variables in globals.css so raw CSS (grain,
 * gradients, meters) can reference them.
 */
const config: Config = {
  content: ["./app/**/*.{ts,tsx}", "./components/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        ground: "#0B0C0E",
        panel: "#141619",
        "panel-2": "#1A1D21",
        hairline: "#23262B",
        "hairline-2": "#2E3238",
        ink: {
          hi: "#ECEDEF",
          mid: "#A0A4AB",
          lo: "#6B7079",
        },
        brass: {
          DEFAULT: "#E0A340",
          deep: "#B87A28",
          dim: "#8A6220",
        },
        ok: "#86C67C",
        warn: "#E0A340",
        fail: "#E5675B",
      },
      fontFamily: {
        display: ["var(--font-display)", "ui-sans-serif", "system-ui"],
        sans: ["var(--font-body)", "ui-sans-serif", "system-ui"],
        mono: ["var(--font-mono)", "ui-monospace", "monospace"],
      },
      fontSize: {
        "2xs": ["0.6875rem", { lineHeight: "1rem", letterSpacing: "0.02em" }],
      },
      borderRadius: {
        panel: "10px",
      },
      boxShadow: {
        panel: "0 1px 0 0 rgba(255,255,255,0.02) inset, 0 8px 30px -12px rgba(0,0,0,0.6)",
        brass: "0 0 0 1px rgba(224,163,64,0.35), 0 8px 24px -8px rgba(224,163,64,0.25)",
      },
      keyframes: {
        "rise-in": {
          "0%": { opacity: "0", transform: "translateY(8px)" },
          "100%": { opacity: "1", transform: "translateY(0)" },
        },
        "meter-pulse": {
          "0%, 100%": { opacity: "0.55" },
          "50%": { opacity: "1" },
        },
        "wave": {
          "0%, 100%": { transform: "scaleY(0.35)" },
          "50%": { transform: "scaleY(1)" },
        },
        "spin-slow": {
          to: { transform: "rotate(360deg)" },
        },
      },
      animation: {
        "rise-in": "rise-in 0.5s cubic-bezier(0.16,1,0.3,1) both",
        "meter-pulse": "meter-pulse 1.6s ease-in-out infinite",
        "spin-slow": "spin-slow 1s linear infinite",
      },
    },
  },
  plugins: [],
};

export default config;
