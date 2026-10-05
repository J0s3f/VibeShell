// Artifact source for the vi-like editor example.
//
// The handler owns everything the primitives cannot: which simulated file is
// open, what a command line means, whether a dirty buffer may be abandoned, and
// what a save proposes to the world. It receives input events for keys no
// binding claimed and semantic events for keys bound to emit_event, and returns
// the new interaction state as app state plus the next declarative view.
//
// It runs inside the sandbox with simulated capabilities only. It never reads a
// host filesystem, never emits terminal control sequences, and never talks to a
// shell.

const SAVE_COMMAND = "w";
const QUIT_COMMAND = "q";
const FORCE_QUIT_COMMAND = "q!";
const WRITE_QUIT_COMMAND = "wq";
const COMMAND_PROMPT = ":";
const SEARCH_PROMPT = "/";

// openFile seeds an interaction with the exact bytes the host resolved from the
// simulated content reference. The host resolves the reference; the handler only
// receives text.
function openFile(name, text, revision) {
  return {
    spec: {
      view: {
        mode: "editor",
        gutter: true,
        status_line: `${name} {dirty} -- {mode} -- {line}/{lines} col {column}`
      },
      mode: "normal",
      default_mode: "normal",
      modes: HANDLER_MODES
    },
    state: {
      buffer: { lines: text.split("\n"), cursor: { line: 0, column: 0 } },
      revision: revision,
      name: name,
      dirty: false
    }
  };
}

// handle processes one event and returns the next result.
export function handle(event, state) {
  if (event.type === "input") {
    return onInput(event, state);
  }
  if (event.type === "semantic") {
    return onSemantic(event, state);
  }
  if (event.type === "resize") {
    return keep(state);
  }
  if (event.type === "timer") {
    return keep(state);
  }
  return keep(state);
}

// onInput owns the prompt lines. Keys bound to primitives never arrive here; the
// handler receives the printable text of its own prompt line, so a search
// pattern and a command are ordinary application state.
function onInput(event, state) {
  const mode = state.mode || "normal";
  const text = event.text || "";
  switch (mode) {
    case "command":
      return echo(state, text);
    case "search":
      return echo(state, text);
    default:
      // A key with no binding and no handling here is what the extension path
      // is for: the host asks the model to extend this artifact rather than
      // guessing.
      return { state: state, extend: { key: event.keys, text: text } };
  }
}

// echo appends typed text to the prompt line and shows it on the status line.
function echo(state, text) {
  const prompt = state.mode === "search" ? SEARCH_PROMPT : COMMAND_PROMPT;
  if (text === "\b" || text === "\u007f") {
    const trimmed = (state.prompt || "").slice(0, -1);
    return { state: { prompt: trimmed, status: prompt + trimmed } };
  }
  const next = (state.prompt || "") + text;
  return { state: { prompt: next, status: prompt + next } };
}

// onSemantic interprets the events the spec emits.
function onSemantic(event, state) {
  switch (event.name) {
    case "enter_append":
      return {
        state: { mode: "insert", cursor: { line: state.buffer.cursor.line, column: state.buffer.cursor.column + 1 } }
      };
    case "open_search":
      return { state: { mode: "search", prompt: "", status: SEARCH_PROMPT } };
    case "open_command":
      return { state: { mode: "command", prompt: "", status: COMMAND_PROMPT } };
    case "cancel_command":
    case "cancel_search":
      return { state: { mode: "normal", prompt: "", status: "" } };
    case "copy_selection":
      return { state: { status: "selection copied", selection: event.selection } };
    case "run_command":
      return runCommand(state, (event.command || "").trim());
    default:
      return { state: state };
  }
}

// runCommand is the application's own command set. It is deliberately small and
// deliberately not part of the runtime: the primitives know nothing about it.
function runCommand(state, command) {
  switch (command) {
    case SAVE_COMMAND:
      return save(state, { exit: false });
    case WRITE_QUIT_COMMAND:
      return save(state, { exit: true });
    case FORCE_QUIT_COMMAND:
      return { state: { mode: "normal", prompt: "", status: "", exited: true, exit_code: 0 } };
    case QUIT_COMMAND:
      if (state.dirty) {
        return { state: { status: "E37: No write since last change (add ! to override)" } };
      }
      return { state: { mode: "normal", prompt: "", status: "", exited: true, exit_code: 0 } };
    default:
      return { state: { status: `E492: Not an editor command: ${command}` } };
  }
}

// save proposes a world mutation. The primitives cannot write; the effect goes
// through the application's commit path, where the expected revision makes a
// concurrent edit a conflict instead of a silent overwrite.
function save(state, options) {
  const result = {
    state: {
      mode: "normal",
      prompt: "",
      status: `${SAVE_COMMAND}: ${state.revision} lines written`,
      dirty: false
    },
    effects: [
      {
        sync: true,
        mutation: {
          type: "update",
          namespace_id: state.namespace,
          path: state.path,
          node_id: state.node,
          expected_rev: state.revision,
          kind: "file"
        }
      }
    ]
  };
  if (options.exit) {
    result.state.exited = true;
    result.state.exit_code = 0;
  }
  return result;
}

function keep(state) {
  return { state: state };
}