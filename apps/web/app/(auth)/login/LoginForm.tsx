"use client";

import { useFormState, useFormStatus } from "react-dom";
import { signIn, demoLogin } from "@/app/actions/auth";

function SubmitButton() {
  const { pending } = useFormStatus();
  return (
    <button type="submit" disabled={pending} className="btn-brass w-full py-2.5">
      {pending ? (
        <>
          <span className="inline-block h-3.5 w-3.5 animate-spin-slow rounded-full border-2 border-brass/30 border-t-brass" />
          Signing in…
        </>
      ) : (
        <>Log in →</>
      )}
    </button>
  );
}

function DemoButton() {
  const { pending } = useFormStatus();
  return (
    <button type="submit" disabled={pending} className="btn w-full py-2.5">
      {pending ? "Setting up…" : "Continue with a demo account"}
    </button>
  );
}

export function LoginForm({ next }: { next: string }) {
  const [state, formAction] = useFormState(signIn, undefined);
  const [demoState, demoAction] = useFormState(async () => demoLogin(), undefined);

  return (
    <div className="space-y-4">
      <form action={formAction} className="space-y-4">
        <input type="hidden" name="next" value={next} />
        <div>
          <label htmlFor="email" className="label mb-2 block">
            Email
          </label>
          <input id="email" name="email" type="email" autoComplete="email" placeholder="you@company.com" className="input" autoFocus />
        </div>
        <div>
          <label htmlFor="password" className="label mb-2 block">
            Password
          </label>
          <input id="password" name="password" type="password" autoComplete="current-password" placeholder="••••••••" className="input" />
        </div>

        {state?.error && (
          <div className="rounded-md border border-fail/30 bg-fail/10 px-3 py-2 text-sm text-fail">{state.error}</div>
        )}

        <SubmitButton />
      </form>

      <div className="flex items-center gap-3 py-1">
        <span className="h-px flex-1 bg-hairline" />
        <span className="text-2xs font-mono uppercase tracking-wider text-ink-lo">or</span>
        <span className="h-px flex-1 bg-hairline" />
      </div>

      <form action={demoAction}>
        <DemoButton />
        {demoState?.error && <div className="mt-2 text-2xs text-fail">{demoState.error}</div>}
      </form>
    </div>
  );
}
