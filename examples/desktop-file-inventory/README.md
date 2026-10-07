# Desktop file inventory (macOS)

Report immediate regular files in a folder you choose, with byte size and modification time. It uses macOS `find` and `stat`; it does not recurse, open files, or upload data. The selected directory’s filenames are returned, so choose a folder whose names are appropriate to share with your connected assistant.

For the harmless `/tmp/tap-desktop-demo` fixtures used in the command examples, follow [the desktop fixture setup](../desktop-support/README.md).

Requires macOS with `/usr/bin/find` and `/usr/bin/stat`. From the TAP-Runtime repository root:

```sh
go run ./host examples/desktop-file-inventory '{"directory":"/tmp/tap-desktop-demo"}'
```

The runner checks that the directory is absolute and refuses `/`. It lists at most 100 immediate files. A missing or unreadable directory produces a useful host-command error. No approval is needed because the declared commands only read metadata.
