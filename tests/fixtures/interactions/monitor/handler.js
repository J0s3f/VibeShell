// Artifact source for the top-like monitor example.
//
// The handler owns the simulated numbers. It never reads a host process table,
// a container counter, or a performance metric: the values are invented here and
// refreshed only when something asks. Sorting is handler logic too, because
// reordering rows is a decision about application data, not a screen primitive;
// the spec emits a sort event and this handler answers with reordered rows and
// the ordering it applied, which the view then reports on its status line.
//
// Refreshes are coalesced by the host: it sends one timer event per visible
// screen at the configured interval, never one per idle session.

const SAMPLE_ROWS = [
  ["101", "session-supervisor", "0.4", "1.2", "sleeping"],
  ["102", "app-registry", "1.8", "3.4", "running"],
  ["103", "research-store", "0.9", "8.1", "running"]
];

const CPU_SORTED = [
  ["102", "app-registry", "1.8", "3.4", "running"],
  ["103", "research-store", "0.9", "8.1", "running"],
  ["101", "session-supervisor", "0.4", "1.2", "sleeping"]
];

const MEM_SORTED = [
  ["103", "research-store", "0.9", "8.1", "running"],
  ["102", "app-registry", "1.8", "3.4", "running"],
  ["101", "session-supervisor", "0.4", "1.2", "sleeping"]
];

const COLUMN_INDEX = { pid: 0, command: 1, cpu: 2, mem: 3, state: 4 };

export function handle(event, state) {
  if (event.type !== "semantic") {
    // Navigation never arrives here: it is bound to cursor and scroll
    // primitives and is handled without the handler running at all.
    return { state: state };
  }
  switch (event.name) {
    case "sort":
      return sortBy(event.fields || {}, state);
    case "refresh":
      return refresh(state);
    case "quit":
      return { state: { status: "", exited: true, exit_code: 0 } };
    default:
      return { state: state };
  }
}

// sortBy reorders the sample data by one declared column. The columns are
// declared in the view, so a key that names a column nobody declared is an
// application error rather than a silently accepted command.
function sortBy(fields, state) {
  const key = fields.column;
  const index = COLUMN_INDEX[key];
  if (index === undefined) {
    return { state: { status: `unknown sort column: ${key}` } };
  }
  const rows = SAMPLE_ROWS.slice();
  rows.sort((left, right) => compareValues(left[index], right[index], fields.order));
  return {
    state: {
      table_rows: rows,
      table_cursor: { line: 0, column: index },
      sort: { column: key, order: fields.order || "ascending" },
      status: `sorted by ${key} ${fields.order || "ascending"}`
    }
  };
}

function compareValues(left, right, order) {
  const leftNumber = Number(left);
  const rightNumber = Number(right);
  let result;
  if (!Number.isNaN(leftNumber) && !Number.isNaN(rightNumber)) {
    result = leftNumber - rightNumber;
  } else {
    result = String(left).localeCompare(String(right));
  }
  return order === "descending" ? -result : result;
}

// refresh produces the next simulated sample. Coalescing and rate limits belong
// to the host; the handler only answers one request with one sample.
function refresh(state) {
  const previous = (state && state.table_rows) || SAMPLE_ROWS;
  const rows = previous.map((row, index) => {
    const next = row.slice();
    next[2] = jitter(next[2], index);
    return next;
  });
  return {
    state: {
      table_rows: rows,
      status: "refreshed simulated sample"
    }
  };
}

// jitter moves a simulated value deterministically so repeated refreshes do
// not repeat the same sample without leaving the plausible range.
function jitter(value, index) {
  const current = Number(value);
  if (Number.isNaN(current)) {
    return value;
  }
  const step = ((index + 1) % 3) * 0.1;
  return (current + step).toFixed(1);
}