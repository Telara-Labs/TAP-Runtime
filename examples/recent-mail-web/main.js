// Usage: recent-mail-web [days]   (default 3, at most 30)
const days = Math.min(Math.max(parseInt(scriptArgs[0] || "3", 10) || 3, 1), 30);
let pageToken, threads = 0, pages = 0;
const senders = new Map();
do {
  const r = tap.call("threads", { query: "in:inbox newer_than:" + days + "d", pageSize: 50, view: "THREAD_VIEW_METADATA_ONLY", ...(pageToken ? { pageToken } : {}) });
  pages++;
  for (const t of r.threads || []) {
    threads++;
    const s = (t.messages && t.messages[0] && t.messages[0].sender) || "";
    const domain = (s.split("@")[1] || s).replace(/>$/, "").toLowerCase();
    senders.set(domain, (senders.get(domain) || 0) + 1);
  }
  pageToken = r.nextPageToken;
} while (pageToken && pages < 4);
const top = [...senders.entries()].sort((a, b) => b[1] - a[1]).slice(0, 3).map(([d, n]) => d + " (" + n + ")");
print(JSON.stringify({ days, threads, pages_read: pages, capped: !!pageToken, sender_domains: senders.size, busiest: top }));
