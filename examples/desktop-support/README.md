# Desktop example fixtures

The desktop examples use paths you choose on your own Mac. To reproduce the package README commands from a fresh checkout, create the disposable demo folder and copy these harmless sample files into it from the TAP-Runtime repository root:

```sh
mkdir -p /tmp/tap-desktop-demo
cp examples/desktop-support/fixtures/* /tmp/tap-desktop-demo/
```

The folder contains a one-pixel PNG, a short RTF document, and a plain-text file for unsupported-format checks. No personal files are involved. The image inspector example can inspect `sample.png`; the document example can extract text from `sample.rtf`; and the inventory example lists all three files.

The `desktop-open-review` example visibly reveals this folder in Finder and requires TAP approval. Only run it when you want Finder to open the disposable demo location.

The adjacent `test_desktop_examples.py` suite uses a fake `tap.exec` broker. It checks program logic without calling macOS commands; it is unit coverage, not proof that native programs ran.
