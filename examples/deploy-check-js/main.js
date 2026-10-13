let namespace = "default";
if (scriptArgs.length > 1) throw new Error("expected at most one JSON input with a namespace");
if (scriptArgs.length === 1) {
  const input = JSON.parse(scriptArgs[0]);
  if (!input || typeof input !== "object" || Array.isArray(input) ||
      Object.keys(input).length !== 1 || typeof input.namespace !== "string") {
    throw new Error("input must contain only namespace");
  }
  namespace = input.namespace;
}
if (!/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(namespace)) {
  throw new Error("namespace must be a valid Kubernetes namespace");
}

const r = tap.exec("kubectl", ["get", "deploy", "-n", namespace, "--context", "minikube", "-o", "json"]);
for (const d of JSON.parse(r.stdout).items.slice(0, 5)) {
  print(d.metadata.name, `${d.status.readyReplicas ?? 0}/${d.spec.replicas}`);
}
for (const [cmd, args] of [["curl", ["-s", "https://example.com"]], ["kubectl", ["delete", "pod", "x", "-n", namespace, "--context", "minikube"]]]) {
  try { tap.exec(cmd, args); print(cmd, "->", "RAN"); } catch (e) { print(cmd, "->", e.message); }
}
