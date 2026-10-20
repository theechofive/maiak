import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";

type ToastKind = "success" | "error";
interface Toast {
  id: number;
  message: string;
  kind: ToastKind;
}

type ShowToast = (message: string, kind?: ToastKind) => void;

const ToastContext = createContext<ShowToast | null>(null);
const TOAST_MS = 5_000;

/** Short save results in a polite live region, so screen readers announce them without moving focus. */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const nextId = useRef(1);

  const dismiss = useCallback((id: number) => {
    setToasts((list) => list.filter((t) => t.id !== id));
  }, []);

  const show = useCallback<ShowToast>(
    (message, kind = "success") => {
      const id = nextId.current++;
      setToasts((list) => [...list, { id, message, kind }]);
      setTimeout(() => dismiss(id), TOAST_MS);
    },
    [dismiss],
  );

  return (
    <ToastContext.Provider value={show}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {toasts.map((t) => (
          <p key={t.id} className={`toast toast-${t.kind}`}>
            <span>{t.message}</span>
            <button
              type="button"
              className="outline secondary"
              aria-label="Dismiss"
              onClick={() => dismiss(t.id)}
            >
              ×
            </button>
          </p>
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
