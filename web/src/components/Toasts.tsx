import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";
import { Toast as ToastView } from "./ui";

type ToastKind = "success" | "error";
interface Toast {
  id: number;
  message: string;
  kind: ToastKind;
}

type ShowToast = (message: string, kind?: ToastKind) => void;

const ToastContext = createContext<ShowToast | null>(null);
const TOAST_MS = 5_000;

/**
 * Short save results in a polite live region, so screen readers announce them without moving
 * focus. Successes leave after 5 s, paused while the pointer is over them; errors stay until
 * dismissed, so nobody misses one.
 */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const nextId = useRef(1);
  const timers = useRef(new Map<number, ReturnType<typeof setTimeout>>());

  const dismiss = useCallback((id: number) => {
    clearTimeout(timers.current.get(id));
    timers.current.delete(id);
    setToasts((list) => list.filter((t) => t.id !== id));
  }, []);

  // While the pointer is over the stack no success toast leaves; the timers restart on leave.
  const hovering = useRef(false);

  const schedule = useCallback(
    (id: number) => {
      clearTimeout(timers.current.get(id));
      timers.current.set(
        id,
        setTimeout(() => dismiss(id), TOAST_MS),
      );
    },
    [dismiss],
  );

  const show = useCallback<ShowToast>(
    (message, kind = "success") => {
      const id = nextId.current++;
      setToasts((list) => [...list, { id, message, kind }]);
      if (kind === "success" && !hovering.current) schedule(id);
    },
    [schedule],
  );

  function pause() {
    hovering.current = true;
    timers.current.forEach(clearTimeout);
    timers.current.clear();
  }

  function resume() {
    hovering.current = false;
    toasts.filter((t) => t.kind === "success").forEach((t) => schedule(t.id));
  }

  return (
    <ToastContext.Provider value={show}>
      {children}
      <div
        role="status"
        aria-live="polite"
        className="fixed right-4 bottom-4 z-20 flex w-[calc(100%-32px)] max-w-sm flex-col gap-2 sm:right-6 sm:bottom-6"
        onMouseEnter={pause}
        onMouseLeave={resume}
      >
        {toasts.map((t) => (
          <ToastView key={t.id} kind={t.kind} onDismiss={() => dismiss(t.id)}>
            {t.message}
          </ToastView>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast(): ShowToast {
  const show = useContext(ToastContext);
  if (!show) throw new Error("useToast must be used inside ToastProvider");
  return show;
}
