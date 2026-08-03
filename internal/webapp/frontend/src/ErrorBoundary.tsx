import { Component, type ErrorInfo, type ReactNode } from "react";

/* The app's floor.
 *
 * React unmounts the whole tree when a render throws and nothing catches it, so
 * before this existed one throw anywhere below <App/> left a blank document
 * with the URL intact — which means a reload reproduces it and the user has no
 * way back. That is a denial of service that another member's CONTENT can
 * reach: round 12's decodePath threw URIError on a link inside a teammate's
 * markdown, and the reader's whole SPA went white, permanently.
 *
 * decodePath is fixed at the source; this is the floor under the next one. It
 * is deliberately the smallest thing that works: no reporting, no retry state
 * machine, no per-route boundaries. Just a page that stays on screen and a way
 * back to a URL that is known to render.
 */
interface Props {
  children: ReactNode;
}
interface State {
  error: Error | null;
}

export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("BearDrive: unhandled render error", error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="mx-auto max-w-lg p-8 text-sm">
        <h1 className="mb-2 text-lg font-semibold">This page didn&rsquo;t load</h1>
        <p className="mb-4 opacity-80">
          Something went wrong rendering this view. The rest of BearDrive is fine.
        </p>
        <p className="mb-4">
          <a className="underline" href="/">
            Go to the project list
          </a>
        </p>
        <pre className="overflow-x-auto rounded bg-black/5 p-3 text-xs dark:bg-white/10">
          {String(this.state.error)}
        </pre>
      </div>
    );
  }
}
