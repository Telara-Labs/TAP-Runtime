# Inspection cost and unchanged inputs

The local inspector runs a bounded candidate scan before invoking the expensive
recognizers. Ordinary ASCII punctuation, markdown labels, URLs and nonsensitive
assignments do not require token or line allocations. False means uncertain and
always selects complete inspection. Metadata profiles retain their address and
webhook inspection, and sensitive ancestors retain full contextual inspection.

The scan checks credential-name words and provider prefixes even inside words.
It conservatively includes all words needed by the assignment, query, header and
qualified-name grammars. Credential-like names, provider prefixes, private-key
headers, encoded query names, userinfo, continuations, non-ASCII and uncertain
syntax select the complete recognizers. A plain YAML mapping head also selects
complete inspection, preserving schema, duplicate-key, alias and tag refusals.
The original punctuation-light proof remains available without decoding JSON
whitespace escapes.

JSON fragments, including objects inside fenced markdown, are parsed in place
once per fragment. Keys and scalar strings use the same candidate scan. Each
object checks duplicates with a fixed sixteen-member array. Numbers are single
digits, nesting is bounded to sixteen, and other encodings select full inspection.
Escaped strings decode at most once into a single builder allocation. Only the
supported JSON escapes are accepted, and Unicode escapes use the complete path.
No unchanged subtree is re-encoded. No normalized document or replacement tree
is allocated for accepted input. The scan is linear with fixed bounds on nesting
and duplicate comparisons. It keeps no state across calls or caller objects.

The root shortcut requires a fresh inspection budget. Recursive callers retain
the original budget accounting. The historically public mutable shape list and
sensitive-name expression are compared with their built-in snapshots before
shortcuts can run. Adding or replacing either disables the proofs.

The frozen source floor remains independent of structured inspection. Literal,
pristine-normalized and current-normalized spans coincide only for stable ASCII.
Only then are spans collected once. Other inputs retain all three views and the
resulting union is enforced after inspection. Candidate uncertainty never drops
source-floor, schema, value-annotation or metadata decisions.

The unoptimized inspector is retained only in a test file as a differential
oracle. Tests compare exact output and detection class in ordinary, sensitive,
schema-data and metadata modes. They include the complete shared corpus, the
same corpus wrapped in markdown with delimiters, realistic response sizes,
random punctuation-heavy text and JSON wrappers, and fuzz where every line
contains both a colon and an equals sign. The behavior golden and ruleset epoch
remain unchanged when outputs are equivalent.
