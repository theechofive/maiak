import { Component, type ErrorInfo, type ReactNode } from "react";
import { Button, ErrorState } from "./ui";

interface State {
  error: Error | null;
}

/** Last line of defence for render errors: a readable message instead of a blank page. */
export default class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error(error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="flex flex-col gap-3 py-10">
        <ErrorState
          title="Something went wrong"
          detail="The page failed to display. Reload it to try again."
        />
        <div>
          <Button variant="secondary" onClick={() => window.location.reload()}>
            Reload the page
          </Button>
        </div>
      </div>
    );
  }
}
