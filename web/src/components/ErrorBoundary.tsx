import { Component, type ErrorInfo, type ReactNode } from "react";

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
      <section role="alert">
        <h1>Something went wrong</h1>
        <p>The page failed to display. Reload it to try again.</p>
        <button type="button" onClick={() => window.location.reload()}>
          Reload the page
        </button>
      </section>
    );
  }
}
