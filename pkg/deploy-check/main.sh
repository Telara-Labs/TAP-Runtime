# Ordinary bash. No SDK, no protocol code.
echo "last 3 commits:"
for line in $(git log --oneline -3 --format=%h); do
  echo "  commit $line"
done
echo "deployments in telara-agents:"
kubectl get deploy -n telara-agents --context minikube -o json \
  | jq -r '.items[] | .metadata.name + " " + (.status.readyReplicas // 0 | tostring) + "/" + (.spec.replicas | tostring)' \
  | head -n 5
echo "attempting things the manifest does not allow:"
curl -s https://example.com && echo "curl ran" || echo "  curl blocked (exit $?)"
kubectl delete pod nonexistent -n telara-agents --context minikube || echo "  delete blocked (exit $?)"
git -C /tmp log --oneline -1 || echo "  git with a leading global flag blocked (exit $?)"
echo hi > /tmp/runner13-escape.txt || echo "  file write blocked"
echo done
