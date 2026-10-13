# Ordinary bash. No SDK, no protocol code.
echo "last 3 commits of this repository:"
for line in $(git log --oneline -3 --format=%h); do
  echo "  commit $line"
done
echo "and of another directory, through a declared global flag:"
git -C "$1" log --oneline -1 --format='  %h' || echo "  refused (exit $?)"
echo "deployments in telara-agents:"
kubectl --context minikube -n telara-agents get deploy -o json \
  | jq -r '.items[] | .metadata.name + " " + (.status.readyReplicas // 0 | tostring) + "/" + (.spec.replicas | tostring)' \
  | head -n 5
echo "piped into a host program:"
echo "lower case in" | tr a-z A-Z
echo "attempting things the manifest does not allow:"
curl -s https://example.com && echo "curl ran" || echo "  curl blocked (exit $?)"
kubectl --context minikube -n telara-agents delete pod nonexistent || echo "  delete gated (exit $?)"
kubectl --context prod get pods || echo "  other cluster blocked (exit $?)"
kubectl --kubeconfig /tmp/other get pods || echo "  undeclared flag blocked (exit $?)"
echo hi > /tmp/runner13-escape.txt || echo "  file write blocked"
echo done
