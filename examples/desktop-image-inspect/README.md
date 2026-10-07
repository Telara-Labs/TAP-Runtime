# Desktop image inspector (macOS)

Read pixel width, height, and format for one image you choose. It calls macOS `sips` and prints the returned metadata. Image bytes are not read into the primitive or sent anywhere.

For the harmless `/tmp/tap-desktop-demo` fixtures used in the command examples, follow [the desktop fixture setup](../desktop-support/README.md).

Requires macOS with `/usr/bin/sips`; supported filename extensions are PNG, JPEG, TIFF, GIF, and HEIC. From the TAP-Runtime repository root:

```sh
go run ./host examples/desktop-image-inspect '{"image":"/tmp/tap-desktop-demo/sample.png"}'
```

The input must be an absolute path with a supported extension. A missing, corrupt, or unsupported image returns an error; non-image files are rejected before invoking `sips`. This example reads only the named file and needs no approval.
