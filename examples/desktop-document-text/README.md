# Desktop document text extraction (macOS)

Extract plain text from one RTF, DOC, or DOCX document you choose. It invokes macOS `textutil -convert txt -stdout`, so it prints the conversion result without creating a converted file.

For the harmless `/tmp/tap-desktop-demo` fixtures used in the command examples, follow [the desktop fixture setup](../desktop-support/README.md).

Requires macOS with `/usr/bin/textutil`. From the TAP-Runtime repository root:

```sh
tap examples/desktop-document-text '{"document":"/tmp/tap-desktop-demo/sample.rtf"}'
```

The input must be an absolute path with an `.rtf`, `.doc`, or `.docx` extension. Choose a document whose text you are comfortable displaying to your assistant: extracted content appears in the run output and may be retained by the client. A missing or malformed document returns a `textutil` error. This example reads only the named document and needs no approval.
