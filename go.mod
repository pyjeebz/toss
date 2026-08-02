module github.com/pyjeebz/toss

go 1.23

require github.com/oklog/ulid/v2 v2.1.0

// Test-only. The reference encoder web/qr.js is checked against; nothing
// outside a _test.go imports it and it is not linked into the binary.
require github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
