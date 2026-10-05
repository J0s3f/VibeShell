// Artifact source for the less-like pager example.
//
// The handler owns the file reference the pager was opened on and nothing else.
// Paging, searching, and the position readout are primitives, so ordinary
// navigation never reaches this code and never costs a model call. The handler
// only answers the semantic events the spec emits: opening the search prompt,
// cancelling it, and quitting.

const SEARCH_PROMPT = "/";

export function handle(event, state) {
  if (event.type === "semantic") {
    switch (event.name) {
      case "open_search":
        return { state: { mode: "search", prompt: "", status: SEARCH_PROMPT } };
      case "cancel_search":
        return { state: { mode: "browse", prompt: "", status: "" } };
      case "quit":
        return { state: { mode: "browse", status: "", exited: true, exit_code: 0 } };
      default:
        return { state: state };
    }
  }
  if (event.type === "input") {
    // Printable keys inside the search prompt accumulate the pattern. The
    // primitives run the search when enter is pressed.
    if ((state.mode || "browse") !== "search") {
      return { state: state, extend: { key: event.keys, text: event.text } };
    }
    const text = event.text || "";
    if (text === "\b" || text === "\u007f") {
      const trimmed = (state.prompt || "").slice(0, -1);
      return { state: { prompt: trimmed, status: SEARCH_PROMPT + trimmed } };
    }
    const next = (state.prompt || "") + text;
    return { state: { prompt: next, status: SEARCH_PROMPT + next } };
  }
  // Resize and timer events change nothing: the frame is derived from the
  // viewport and the exact buffer content the host resolved.
  return { state: state };
}