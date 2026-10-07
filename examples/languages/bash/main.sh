# The shell and jq run inside TAP's WASM guest. cat requests the declared read.
set -euo pipefail
[ "$#" -eq 1 ] || exit 1
status=$(echo "$1" | jq -r 'if type == "object" and keys == ["status"] and (.status == "open" or .status == "done") then .status else error("expected status open or done") end')
cat examples/languages/fixtures/tasks.json | jq '
  if type != "array" then error("expected task array")
  elif any(.[]; type != "object" or (.id | type) != "string" or (.title | type) != "string" or (.status != "open" and .status != "done")) then error("invalid task")
  else map(select(.status == "'"$status"'")) | {status:"'"$status"'", count:length, items:map({id,title})}
  end'
