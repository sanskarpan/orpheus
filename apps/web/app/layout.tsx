import type { Metadata } from "next";
import { body, display, mono } from "@/lib/fonts";
import "./globals.css";

export const metadata: Metadata = {
  title: "Orpheus — Studio Console",
  description: "Operate the Orpheus audio-processing platform: upload, transcribe, and inspect every job.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className={`${display.variable} ${body.variable} ${mono.variable}`}>
      <body className="relative min-h-screen">
        <div className="relative z-10">{children}</div>
      </body>
    </html>
  );
}
