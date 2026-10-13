# The same primitive in bash.

for alias in $(tap tools); do
  case "$alias" in
    threads) tap call threads '{"query":"in:inbox newer_than:3d","pageSize":25}' | jq -r '"threads on first page: " + (.threads | length | tostring)' ;;
    emails)  tap call emails  '{"query":"in:inbox newer_than:3d","max_results":25}' | jq -r '"messages on first page: " + (.emails | length | tostring)' ;;
  esac
done
