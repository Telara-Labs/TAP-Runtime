const r = tap.exec("kubectl", ["get", "deploy", "-n", "telara-agents", "--context", "minikube", "-o", "json"]);
for (const d of JSON.parse(r.stdout).items.slice(0, 5)) {
  print(d.metadata.name, `${d.status.readyReplicas ?? 0}/${d.spec.replicas}`);
}
for (const [cmd, args] of [["curl", ["-s", "https://example.com"]], ["kubectl", ["delete", "pod", "x", "--context", "minikube"]]]) {
  print(cmd, "->", tap.exec(cmd, args).refused ?? "RAN");
}
