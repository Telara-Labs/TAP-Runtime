# Reveal a folder for desktop review (macOS)

Ask Finder to reveal one folder you choose, so you can inspect it yourself. This is a visible desktop action; TAP requires approval before it runs. It uses only `open -R` and does not read or change the folder contents.

For the harmless `/tmp/tap-desktop-demo` fixtures used in the command examples, follow [the desktop fixture setup](../desktop-support/README.md).

Requires macOS with `/usr/bin/open` and an interactive Finder session. First create a disposable folder, then run from the TAP-Runtime repository root:

```sh
mkdir -p /tmp/tap-review-demo
# Review the manifest first: this flag grants the visible Finder effect.
tap --approve examples/desktop-open-review '{"folder":"/tmp/tap-review-demo"}'
```

The path must be absolute and cannot be `/`. The program requests a human review; it does not click controls, capture the screen, or automate Finder beyond revealing the selected folder. Do not point it at private work unless you intend to open that location in Finder.
