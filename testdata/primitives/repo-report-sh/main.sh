# Ordinary redirection. The host decides whether each file may be touched.
tap fetch https://api.github.com/repos/mvdan/sh | jq -r '.full_name + " " + .license.spdx_id' > out/report-sh.txt || echo "write refused"
cat out/report-sh.txt || echo "read refused"
cat /etc/hosts || echo "read outside refused"
echo x > /tmp/tap-escape.txt || echo "write outside refused"
tap fetch https://example.com/ || echo "other origin refused"
