"use client";

import { useEffect, useState } from "react";

/** A ticking UTC clock in the top strip — a console readout detail. */
export function LiveClock() {
  const [now, setNow] = useState<string>("--:--:--");

  useEffect(() => {
    const tick = () => {
      const d = new Date();
      setNow(
        d.toLocaleTimeString("en-GB", {
          hour: "2-digit",
          minute: "2-digit",
          second: "2-digit",
          timeZone: "UTC",
        }),
      );
    };
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, []);

  return (
    <span className="tnum text-xs text-ink-mid">
      {now}
      <span className="ml-1 text-ink-lo">UTC</span>
    </span>
  );
}
