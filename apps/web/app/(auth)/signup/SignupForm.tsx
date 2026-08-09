"use client";

import { useFormState, useFormStatus } from "react-dom";
import { signUp } from "@/app/actions/auth";

function SubmitButton() {
  const { pending } = useFormStatus();
  return (
    <button type="submit" disabled={pending} className="btn-brass w-full py-2.5">
      {pending ? (
        <>
          <span className="inline-block h-3.5 w-3.5 animate-spin-slow rounded-full border-2 border-brass/30 border-t-brass" />
          Creating your workspace…
        </>
      ) : (
        <>Create account →</>
      )}
    </button>
  );
}

export function SignupForm({ next }: { next: string }) {
  const [state, formAction] = useFormState(signUp, undefined);

  return (
    <form action={formAction} className="space-y-4">
      <input type="hidden" name="next" value={next} />
      <div className="grid grid-cols-2 gap-3">
        <div>
          <label htmlFor="name" className="label mb-2 block">
            Name
          </label>
          <input id="name" name="name" type="text" autoComplete="name" placeholder="Ada Lovelace" className="input" autoFocus />
        </div>
        <div>
          <label htmlFor="workspace" className="label mb-2 block">
            Workspace <span className="text-ink-lo">(optional)</span>
          </label>
          <input id="workspace" name="workspace" type="text" placeholder="Acme Audio" className="input" />
        </div>
      </div>
      <div>
        <label htmlFor="email" className="label mb-2 block">
          Email
        </label>
        <input id="email" name="email" type="email" autoComplete="email" placeholder="you@company.com" className="input" />
      </div>
      <div>
        <label htmlFor="password" className="label mb-2 block">
          Password
        </label>
        <input id="password" name="password" type="password" autoComplete="new-password" placeholder="At least 8 characters" className="input" />
      </div>

      {state?.error && (
        <div className="rounded-md border border-fail/30 bg-fail/10 px-3 py-2 text-sm text-fail">{state.error}</div>
      )}

      <SubmitButton />
      <p className="text-xs text-ink-lo">
        We'll spin up a real workspace with its own org and API key — no key to paste, nothing to configure.
      </p>
    </form>
  );
}
