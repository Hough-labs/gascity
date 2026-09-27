package fsys

import "errors"

// ErrFreeBytesUnsupported is returned by [FreeBytes] on platforms that have
// no free-space query.
var ErrFreeBytesUnsupported = errors.New("free-space query unsupported on this platform")
