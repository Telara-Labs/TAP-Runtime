# Runs anywhere the runner runs. echo, cat, head, wc and jq are built in.
for w in one two three; do echo "{\"word\":\"$w\"}"; done | jq -r '.word'
echo '{"greeting":"hello from bash"}' | jq -r '.greeting'
