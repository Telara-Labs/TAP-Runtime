// TAP removes types, then runs this source inside QuickJS's WASM sandbox.
interface Task { id: string; title: string; status: "open" | "done" }
interface Input { status: Task["status"] }
if (scriptArgs.length !== 1) throw new Error("expected one JSON input");
const input = JSON.parse(scriptArgs[0]) as Input;
if (!input || typeof input !== "object" || Array.isArray(input) ||
    Object.keys(input).length !== 1 || !["open", "done"].includes(input.status)) {
  throw new Error("expected status open or done");
}
const tasks = JSON.parse(tap.read("examples/languages/fixtures/tasks.json")) as Task[];
if (!Array.isArray(tasks) || tasks.some(row => !row || typeof row !== "object" ||
    typeof row.id !== "string" || typeof row.title !== "string" ||
    !["open", "done"].includes(row.status))) {
  throw new Error("invalid task array");
}
const items = tasks.filter(row => row.status === input.status).map(row => ({id: row.id, title: row.title}));
print(JSON.stringify({status: input.status, count: items.length, items}));
